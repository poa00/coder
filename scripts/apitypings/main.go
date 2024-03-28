package main

import (
	"bytes"
	"context"
	"fmt"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/fatih/structtag"
	"golang.org/x/exp/slices"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/tools/go/packages"
	"golang.org/x/xerrors"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/sloghuman"
	"github.com/coder/coder/v2/coderd/util/slice"
)

var (
	// baseDirs are the directories to introspect for types to generate.
	baseDirs = [...]string{"./codersdk", "./codersdk/healthsdk"}
	// externalTypes are types that are not in the baseDirs, but we want to
	// support. These are usually types that are used in the baseDirs.
	// Do not include things like "Database", as that would break the idea
	// of splitting db and api types.
	// Only include dirs that are client facing packages.
	externalTypePkgs = [...]string{
		"./coderd/healthcheck/health",
		// CLI option types:
		"github.com/coder/serpent",
	}
	indent = "  "
)

func main() {
	ctx := context.Background()
	log := slog.Make(sloghuman.Sink(os.Stderr))

	external := []*Generator{}
	for _, dir := range externalTypePkgs {
		extGen, err := ParseDirectory(ctx, log, dir)
		if err != nil {
			log.Fatal(ctx, fmt.Sprintf("parse external directory %s: %s", dir, err.Error()))
		}
		extGen.onlyOptIn = true
		external = append(external, extGen)
	}

	_, _ = fmt.Print("// Code generated by 'make site/src/api/typesGenerated.ts'. DO NOT EDIT.\n\n")
	for _, baseDir := range baseDirs {
		_, _ = fmt.Printf("// The code below is generated from %s.\n\n", strings.TrimPrefix(baseDir, "./"))
		output, err := Generate(baseDir, external...)
		if err != nil {
			log.Fatal(ctx, err.Error())
		}

		// Just cat the output to a file to capture it
		_, _ = fmt.Print(output, "\n\n")
	}

	for i, ext := range external {
		var ts *TypescriptTypes
		for {
			var err error
			start := len(ext.allowList)
			ts, err = ext.generateAll()
			if err != nil {
				log.Fatal(ctx, fmt.Sprintf("generate external: %s", err.Error()))
			}
			if len(ext.allowList) != start {
				// This is so dumb, but basically the allowList can grow, and if
				// it does, we need to regenerate.
				continue
			}
			break
		}

		dir := externalTypePkgs[i]
		_, _ = fmt.Printf("// The code below is generated from %s.\n\n", strings.TrimPrefix(dir, "./"))
		_, _ = fmt.Print(ts.String(), "\n\n")
	}
}

func Generate(directory string, externals ...*Generator) (string, error) {
	ctx := context.Background()
	log := slog.Make(sloghuman.Sink(os.Stderr))
	gen, err := GenerateFromDirectory(ctx, log, directory, externals...)
	if err != nil {
		return "", err
	}

	// Just cat the output to a file to capture it
	return gen.cachedResult.String(), nil
}

// TypescriptTypes holds all the code blocks created.
type TypescriptTypes struct {
	// Each entry is the type name, and it's typescript code block.
	Types    map[string]string
	Enums    map[string]string
	Generics map[string]string
}

// String just combines all the codeblocks.
func (t TypescriptTypes) String() string {
	var s strings.Builder

	sortedTypes := make([]string, 0, len(t.Types))
	sortedEnums := make([]string, 0, len(t.Enums))
	sortedGenerics := make([]string, 0, len(t.Generics))

	for k := range t.Types {
		sortedTypes = append(sortedTypes, k)
	}
	for k := range t.Enums {
		sortedEnums = append(sortedEnums, k)
	}
	for k := range t.Generics {
		sortedGenerics = append(sortedGenerics, k)
	}

	sort.Strings(sortedTypes)
	sort.Strings(sortedEnums)
	sort.Strings(sortedGenerics)

	for _, k := range sortedTypes {
		v := t.Types[k]
		_, _ = s.WriteString(v)
		_, _ = s.WriteRune('\n')
	}

	for _, k := range sortedEnums {
		v := t.Enums[k]
		_, _ = s.WriteString(v)
		_, _ = s.WriteRune('\n')
	}

	for _, k := range sortedGenerics {
		v := t.Generics[k]
		_, _ = s.WriteString(v)
		_, _ = s.WriteRune('\n')
	}

	return strings.TrimRight(s.String(), "\n")
}

func ParseDirectory(ctx context.Context, log slog.Logger, directory string, externals ...*Generator) (*Generator, error) {
	g := &Generator{
		log:       log,
		builtins:  make(map[string]string),
		externals: externals,
	}
	err := g.parsePackage(ctx, directory)
	if err != nil {
		return nil, xerrors.Errorf("parse package %q: %w", directory, err)
	}

	return g, nil
}

// GenerateFromDirectory will return all the typescript code blocks for a directory
func GenerateFromDirectory(ctx context.Context, log slog.Logger, directory string, externals ...*Generator) (*Generator, error) {
	g, err := ParseDirectory(ctx, log, directory, externals...)
	if err != nil {
		return nil, err
	}

	codeBlocks, err := g.generateAll()
	if err != nil {
		return nil, xerrors.Errorf("generate package %q: %w", directory, err)
	}
	g.cachedResult = codeBlocks

	return g, nil
}

type Generator struct {
	// Package we are scanning.
	pkg *packages.Package
	log slog.Logger

	// allowList if set only generates types in the allow list.
	// This is kinda a hack to get around the fact that external types
	// only should generate referenced types, and multiple packages can
	// reference the same external types.
	onlyOptIn bool
	allowList []string

	// externals are other packages referenced. Optional
	externals []*Generator

	// builtins is kinda a hack to get around the fact that using builtin
	// generic constraints is common. We want to support them even though
	// they are external to our package.
	// It is also a string because the builtins are not proper go types. Meaning
	// if you inspect the types, they are not "correct". Things like "comparable"
	// cannot be implemented in go. So they are a first class thing that we just
	// have to make a static string for ¯\_(ツ)_/¯
	builtins map[string]string

	cachedResult *TypescriptTypes
}

// parsePackage takes a list of patterns such as a directory, and parses them.
func (g *Generator) parsePackage(ctx context.Context, patterns ...string) error {
	cfg := &packages.Config{
		// Just accept the fact we need these flags for what we want. Feel free to add
		// more, it'll just increase the time it takes to parse.
		Mode: packages.NeedTypes | packages.NeedName | packages.NeedTypesInfo |
			packages.NeedTypesSizes | packages.NeedSyntax,
		Tests:   false,
		Context: ctx,
	}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return xerrors.Errorf("load package: %w", err)
	}

	// Only support 1 package for now. We can expand it if we need later, we
	// just need to hook up multiple packages in the generator.
	if len(pkgs) != 1 {
		return xerrors.Errorf("expected 1 package, found %d", len(pkgs))
	}

	g.pkg = pkgs[0]
	return nil
}

// generateAll will generate for all types found in the pkg
func (g *Generator) generateAll() (*TypescriptTypes, error) {
	m := &Maps{
		Structs:      make(map[string]string),
		Generics:     make(map[string]string),
		Enums:        make(map[string]types.Object),
		EnumConsts:   make(map[string][]*types.Const),
		IgnoredTypes: make(map[string]struct{}),
		AllowedTypes: make(map[string]struct{}),
	}

	for _, a := range g.allowList {
		m.AllowedTypes[strings.TrimSpace(a)] = struct{}{}
	}

	// Look for comments that indicate to ignore a type for typescript generation.
	ignoreRegex := regexp.MustCompile("@typescript-ignore[:]?(?P<ignored_types>.*)")
	for _, file := range g.pkg.Syntax {
		for _, comment := range file.Comments {
			for _, line := range comment.List {
				text := line.Text
				matches := ignoreRegex.FindStringSubmatch(text)
				ignored := ignoreRegex.SubexpIndex("ignored_types")
				if len(matches) >= ignored && matches[ignored] != "" {
					arr := strings.Split(matches[ignored], ",")
					for _, s := range arr {
						m.IgnoredTypes[strings.TrimSpace(s)] = struct{}{}
					}
				}
			}
		}
	}

	// This allows opt-in generation, instead of opt-out.
	allowRegex := regexp.MustCompile("@typescript-generate[:]?(?P<allowed_types>.*)")
	for _, file := range g.pkg.Syntax {
		for _, comment := range file.Comments {
			for _, line := range comment.List {
				text := line.Text
				matches := allowRegex.FindStringSubmatch(text)
				allowed := allowRegex.SubexpIndex("allowed_types")
				if len(matches) >= allowed && matches[allowed] != "" {
					arr := strings.Split(matches[allowed], ",")
					for _, s := range arr {
						m.AllowedTypes[strings.TrimSpace(s)] = struct{}{}
					}
				}
			}
		}
	}

	for _, n := range g.pkg.Types.Scope().Names() {
		obj := g.pkg.Types.Scope().Lookup(n)
		err := g.generateOne(m, obj)
		if err != nil {
			return nil, xerrors.Errorf("%q: %w", n, err)
		}
	}

	// Add the builtins
	for n, value := range g.builtins {
		if value != "" {
			m.Generics[n] = value
		}
	}

	// Write all enums
	enumCodeBlocks := make(map[string]string)
	for name, v := range m.Enums {
		var values []string
		for _, elem := range m.EnumConsts[name] {
			// TODO: If we have non string constants, we need to handle that
			//		here.
			values = append(values, elem.Val().String())
		}
		sort.Strings(values)
		var s strings.Builder
		_, _ = s.WriteString(g.posLine(v))
		joined := strings.Join(values, " | ")
		if joined == "" {
			// It's possible an enum has no values.
			joined = "never"
		}
		_, _ = s.WriteString(fmt.Sprintf("export type %s = %s\n",
			name, joined,
		))

		var pluralName string
		if strings.HasSuffix(name, "s") {
			pluralName = name + "es"
		} else {
			pluralName = name + "s"
		}

		// Generate array used for enumerating all possible values.
		_, _ = s.WriteString(fmt.Sprintf("export const %s: %s[] = [%s]\n",
			pluralName, name, strings.Join(values, ", "),
		))

		enumCodeBlocks[name] = s.String()
	}

	return &TypescriptTypes{
		Types:    m.Structs,
		Enums:    enumCodeBlocks,
		Generics: m.Generics,
	}, nil
}

type Maps struct {
	Structs      map[string]string
	Generics     map[string]string
	Enums        map[string]types.Object
	EnumConsts   map[string][]*types.Const
	IgnoredTypes map[string]struct{}
	AllowedTypes map[string]struct{}
}

// objName prepends the package name of a type if it is outside of codersdk.
func objName(obj types.Object) string {
	if pkgName := obj.Pkg().Name(); pkgName != "codersdk" && pkgName != "healthsdk" {
		return cases.Title(language.English).String(pkgName) + obj.Name()
	}
	return obj.Name()
}

func (g *Generator) generateOne(m *Maps, obj types.Object) error {
	if obj == nil || obj.Type() == nil {
		// This would be weird, but it is if the package does not have the type def.
		return nil
	}

	// Exclude ignored types
	if _, ok := m.IgnoredTypes[obj.Name()]; ok {
		return nil
	}

	// If we have allowed types, only allow those to be generated.
	if _, ok := m.AllowedTypes[obj.Name()]; (len(m.AllowedTypes) > 0 || g.onlyOptIn) && !ok {
		// Allow constants to pass through, they are only included if the enum
		// is allowed.
		_, ok := obj.(*types.Const)
		if !ok {
			return nil
		}
	}

	objectName := objName(obj)

	switch obj := obj.(type) {
	// All named types are type declarations
	case *types.TypeName:
		named, ok := obj.Type().(*types.Named)
		if !ok {
			panic("all typename should be named types")
		}
		switch underNamed := named.Underlying().(type) {
		case *types.Struct:
			// type <Name> struct
			// Structs are obvious.
			codeBlock, err := g.buildStruct(obj, underNamed)
			if err != nil {
				return xerrors.Errorf("generate %q: %w", objectName, err)
			}
			m.Structs[objectName] = codeBlock
		case *types.Basic:
			// type <Name> string
			// These are enums. Store to expand later.
			m.Enums[objectName] = obj
		case *types.Map, *types.Array, *types.Slice:
			// Declared maps that are not structs are still valid codersdk objects.
			// Handle them custom by calling 'typescriptType' directly instead of
			// iterating through each struct field.
			// These types support no json/typescript tags.
			// These are **NOT** enums, as a map in Go would never be used for an enum.
			ts, err := g.typescriptType(obj.Type().Underlying())
			if err != nil {
				return xerrors.Errorf("(map) generate %q: %w", objectName, err)
			}

			var str strings.Builder
			_, _ = str.WriteString(g.posLine(obj))
			if ts.AboveTypeLine != "" {
				_, _ = str.WriteString(ts.AboveTypeLine)
				_, _ = str.WriteRune('\n')
			}
			// Use similar output syntax to enums.
			_, _ = str.WriteString(fmt.Sprintf("export type %s = %s\n", objectName, ts.ValueType))
			m.Structs[objectName] = str.String()
		case *types.Interface:
			// Interfaces are used as generics. Non-generic interfaces are
			// not supported.
			if underNamed.NumEmbeddeds() == 1 {
				union, ok := underNamed.EmbeddedType(0).(*types.Union)
				if !ok {
					// If the underlying is not a union, but has 1 type. It's
					// just that one type.
					union = types.NewUnion([]*types.Term{
						// Set the tilde to true to support underlying.
						// Doesn't actually affect our generation.
						types.NewTerm(true, underNamed.EmbeddedType(0)),
					})
				}

				block, err := g.buildUnion(obj, union)
				if err != nil {
					return xerrors.Errorf("generate union %q: %w", objectName, err)
				}
				m.Generics[objectName] = block
			}
		case *types.Signature:
		// Ignore named functions.
		default:
			// If you hit this error, you added a new unsupported named type.
			// The easiest way to solve this is add a new case above with
			// your type and a TODO to implement it.
			return xerrors.Errorf("unsupported named type %q", underNamed.String())
		}
	case *types.Var:
		// TODO: Are any enums var declarations? This is also codersdk.Me.
	case *types.Const:
		// We only care about named constant types, since they are enums
		if named, ok := obj.Type().(*types.Named); ok {
			enumObjName := objName(named.Obj())
			m.EnumConsts[enumObjName] = append(m.EnumConsts[enumObjName], obj)
		}
	case *types.Func:
		// Noop
	default:
		_, _ = fmt.Println(objectName)
	}
	return nil
}

func (g *Generator) posLine(obj types.Object) string {
	file := g.pkg.Fset.File(obj.Pos())
	// Do not use filepath, as that changes behavior based on OS
	return fmt.Sprintf("// From %s\n", path.Join(obj.Pkg().Name(), filepath.Base(file.Name())))
}

// buildStruct just prints the typescript def for a type.
func (g *Generator) buildUnion(obj types.Object, st *types.Union) (string, error) {
	var s strings.Builder
	_, _ = s.WriteString(g.posLine(obj))

	allTypes := make([]string, 0, st.Len())
	var optional bool
	for i := 0; i < st.Len(); i++ {
		term := st.Term(i)
		scriptType, err := g.typescriptType(term.Type())
		if err != nil {
			return "", xerrors.Errorf("union %q for %q failed to get type: %w", st.String(), obj.Name(), err)
		}
		allTypes = append(allTypes, scriptType.ValueType)
		optional = optional || scriptType.Optional
	}

	if optional {
		allTypes = append(allTypes, "null")
	}

	allTypes = slice.Unique(allTypes)

	_, _ = s.WriteString(fmt.Sprintf("export type %s = %s\n", objName(obj), strings.Join(allTypes, " | ")))

	return s.String(), nil
}

type structTemplateState struct {
	PosLine   string
	Name      string
	Fields    []string
	Generics  []string
	Extends   string
	AboveLine string
}

const structTemplate = `{{ .PosLine -}}
{{ if .AboveLine }}{{ .AboveLine }}
{{ end }}export interface {{ .Name }}{{ if .Generics }}<{{ join .Generics ", " }}>{{ end }}{{ if .Extends }} extends {{ .Extends }}{{ end }} {
{{ join .Fields "\n"}}
}
`

// buildStruct just prints the typescript def for a type.
func (g *Generator) buildStruct(obj types.Object, st *types.Struct) (string, error) {
	state := structTemplateState{}
	tpl := template.New("struct")
	tpl.Funcs(template.FuncMap{
		"join": strings.Join,
	})
	tpl, err := tpl.Parse(structTemplate)
	if err != nil {
		return "", xerrors.Errorf("parse struct template: %w", err)
	}

	state.PosLine = g.posLine(obj)
	state.Name = objName(obj)

	// Handle named embedded structs in the codersdk package via extension.
	var extends []string
	extendedFields := make(map[int]bool)
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		tag := reflect.StructTag(st.Tag(i))
		// Adding a json struct tag causes the json package to consider
		// the field unembedded.
		if field.Embedded() && tag.Get("json") == "" && field.Pkg().Name() == "codersdk" {
			extendedFields[i] = true
			extends = append(extends, field.Name())
		}
	}
	if len(extends) > 0 {
		state.Extends = strings.Join(extends, ", ")
	}

	genericsUsed := make(map[string]string)
	// For each field in the struct, we print 1 line of the typescript interface
	for i := 0; i < st.NumFields(); i++ {
		if extendedFields[i] {
			continue
		}
		field := st.Field(i)
		tag := reflect.StructTag(st.Tag(i))
		tags, err := structtag.Parse(string(tag))
		if err != nil {
			panic("invalid struct tags on type " + obj.String())
		}

		if !field.Exported() {
			continue
		}

		// Use the json name if present
		jsonTag, err := tags.Get("json")
		var (
			jsonName     string
			jsonOptional bool
		)
		if err == nil {
			if jsonTag.Name == "-" {
				// Completely ignore this field.
				continue
			}
			jsonName = jsonTag.Name
			if len(jsonTag.Options) > 0 && jsonTag.Options[0] == "omitempty" {
				jsonOptional = true
			}
		}
		if jsonName == "" {
			jsonName = field.Name()
		}

		// Infer the type.
		tsType, err := g.typescriptType(field.Type())
		if err != nil {
			return "", xerrors.Errorf("typescript type: %w", err)
		}

		// If a `typescript:"string"` exists, we take this, and ignore what we
		// inferred.
		typescriptTag, err := tags.Get("typescript")
		if err == nil {
			if err == nil && typescriptTag.Name == "-" {
				// Completely ignore this field.
				continue
			} else if typescriptTag.Name != "" {
				tsType = TypescriptType{
					ValueType: typescriptTag.Name,
				}
			}

			// If you specify `typescript:",notnull"` then mark the type as not
			// optional.
			if len(typescriptTag.Options) > 0 && typescriptTag.Options[0] == "notnull" {
				tsType.Optional = false
			}
		}

		optional := ""
		if jsonOptional || tsType.Optional {
			optional = "?"
		}
		valueType := tsType.ValueType
		if tsType.GenericValue != "" {
			valueType = tsType.GenericValue
			// This map we are building is just gathering all the generics used
			// by our fields. We will use this map for our export type line.
			// This isn't actually required since we can get it from the obj
			// itself, but this ensures we actually use all the generic fields
			// we place in the export line. If we are missing one from this map,
			// that is a developer error. And we might as well catch it.
			for name, constraint := range tsType.GenericTypes {
				if _, ok := genericsUsed[name]; ok {
					// Don't add a generic twice
					// TODO: We should probably check that the generic mapping is
					// 	not a different type. Like 'T' being referenced to 2 different
					//	constraints. I don't think this is possible though in valid
					// 	go, so I'm going to ignore this for now.
					continue
				}
				genericsUsed[name] = constraint
			}
		}

		if tsType.AboveTypeLine != "" {
			// Just append these as fields. We should fix this later.
			state.Fields = append(state.Fields, tsType.AboveTypeLine)
		}
		state.Fields = append(state.Fields, fmt.Sprintf("%sreadonly %s%s: %s", indent, jsonName, optional, valueType))
	}

	// This is implemented to ensure the correct order of generics on the
	// top level structure. Ordering of generic fields is important, and
	// we want to match the same order as Golang. The gathering of generic types
	// from our fields does not guarantee the order.
	named, ok := obj.(*types.TypeName)
	if !ok {
		return "", xerrors.Errorf("generic param ordering undefined on %q", obj.Name())
	}

	namedType, ok := named.Type().(*types.Named)
	if !ok {
		return "", xerrors.Errorf("generic param %q unexpected type %q", obj.Name(), named.Type().String())
	}

	// Ensure proper generic param ordering
	params := namedType.TypeParams()
	for i := 0; i < params.Len(); i++ {
		param := params.At(i)
		name := param.String()

		constraint, ok := genericsUsed[param.String()]
		if !ok {
			// If this error is thrown, it is because you have defined a
			// generic field on a structure, but did not use it in your
			// fields. If this happens, remove the unused generic on
			// the top level structure. We **technically** can implement
			// this still, but it's not a case we need to support.
			// Example:
			//	type Foo[A any] struct {
			//	  Bar string
			//	}
			return "", xerrors.Errorf("generic param %q missing on %q, fix your data structure", name, obj.Name())
		}

		state.Generics = append(state.Generics, fmt.Sprintf("%s extends %s", name, constraint))
	}

	data := bytes.NewBuffer(make([]byte, 0))
	err = tpl.Execute(data, state)
	if err != nil {
		return "", xerrors.Errorf("execute struct template: %w", err)
	}
	return data.String(), nil
}

type TypescriptType struct {
	// GenericTypes is a map of generic name to actual constraint.
	// We return these, so we can bubble them up if we are recursively traversing
	// a nested structure. We duplicate these at the top level.
	// Example: 'C = comparable'.
	GenericTypes map[string]string
	// GenericValue is the value using the Generic name, rather than the constraint.
	// This is only useful if you can use the generic syntax. Things like maps
	// don't currently support this, and will use the ValueType instead.
	// Example:
	//	Given the Golang
	//	  type Foo[C comparable] struct {
	//		  Bar C
	//	  }
	// 	The field `Bar` will return:
	//	  TypescriptType {
	//	    ValueType: "comparable",
	//	    GenericValue: "C",
	//	    GenericTypes: map[string]string{
	//		  "C":"comparable"
	//	    }
	//    }
	GenericValue string
	// ValueType is the typescript value type. This is the actual type or
	// generic constraint. This can **always** be used without special handling.
	ValueType string
	// AboveTypeLine lets you put whatever text you want above the typescript
	// type line.
	AboveTypeLine string
	// Optional indicates the value is an optional field in typescript.
	Optional bool
}

// typescriptType this function returns a typescript type for a given
// golang type.
// Eg:
//
//	[]byte returns "string"
func (g *Generator) typescriptType(ty types.Type) (TypescriptType, error) {
	switch ty := ty.(type) {
	case *types.Basic:
		bs := ty
		// All basic literals (string, bool, int, etc).
		switch {
		case bs.Info()&types.IsNumeric > 0:
			return TypescriptType{ValueType: "number"}, nil
		case bs.Info()&types.IsBoolean > 0:
			return TypescriptType{ValueType: "boolean"}, nil
		case bs.Kind() == types.Byte:
			// TODO: @emyrk What is a byte for typescript? A string? A uint8?
			return TypescriptType{ValueType: "number", AboveTypeLine: indentedComment("This is a byte in golang")}, nil
		default:
			return TypescriptType{ValueType: bs.Name()}, nil
		}
	case *types.Struct:
		// This handles anonymous structs. This should never happen really.
		// If you require this, either change your datastructures, or implement
		// anonymous structs here.
		// Such as:
		//  type Name struct {
		//	  Embedded struct {
		//		  Field string `json:"field"`
		//	  }
		//  }
		return TypescriptType{
			ValueType: "any",
			AboveTypeLine: fmt.Sprintf("%s\n%s",
				indentedComment("Embedded anonymous struct, please fix by naming it"),
				// Linter needs to be disabled here, or else it will complain about the "any" type.
				indentedComment("eslint-disable-next-line @typescript-eslint/no-explicit-any -- Anonymously embedded struct"),
			),
		}, nil
	case *types.Map:
		// map[string][string] -> Record<string, string>
		m := ty
		keyType, err := g.typescriptType(m.Key())
		if err != nil {
			return TypescriptType{}, xerrors.Errorf("map key: %w", err)
		}
		valueType, err := g.typescriptType(m.Elem())
		if err != nil {
			return TypescriptType{}, xerrors.Errorf("map key: %w", err)
		}

		aboveTypeLine := keyType.AboveTypeLine
		if aboveTypeLine != "" && valueType.AboveTypeLine != "" {
			aboveTypeLine = aboveTypeLine + "\n"
		}
		aboveTypeLine = aboveTypeLine + valueType.AboveTypeLine

		mergeGens := keyType.GenericTypes
		for k, v := range valueType.GenericTypes {
			mergeGens[k] = v
		}
		return TypescriptType{
			ValueType:     fmt.Sprintf("Record<%s, %s>", keyType.ValueType, valueType.ValueType),
			AboveTypeLine: aboveTypeLine,
			GenericTypes:  mergeGens,
		}, nil
	case *types.Slice, *types.Array:
		// Slice/Arrays are pretty much the same.
		type hasElem interface {
			Elem() types.Type
		}

		arr, _ := ty.(hasElem)
		switch {
		// When type checking here, just use the string. You can cast it
		// to a types.Basic and get the kind if you want too :shrug:
		case arr.Elem().String() == "byte":
			// All byte arrays are strings on the typescript.
			// Is this ok?
			return TypescriptType{ValueType: "string"}, nil
		default:
			// By default, just do an array of the underlying type.
			underlying, err := g.typescriptType(arr.Elem())
			if err != nil {
				return TypescriptType{}, xerrors.Errorf("array: %w", err)
			}
			genValue := ""
			if underlying.GenericValue != "" {
				genValue = underlying.GenericValue + "[]"
			}
			return TypescriptType{
				ValueType:     underlying.ValueType + "[]",
				GenericValue:  genValue,
				AboveTypeLine: underlying.AboveTypeLine,
				GenericTypes:  underlying.GenericTypes,
			}, nil
		}
	case *types.Named:
		n := ty

		// These are external named types that we handle uniquely.
		// This is unfortunate, but our current code assumes all defined
		// types are enums, but these are really just basic primitives.
		// We would need to add more logic to determine this, but for now
		// just hard code them.
		switch n.String() {
		case "github.com/coder/serpent.Regexp":
			return TypescriptType{ValueType: "string"}, nil
		case "github.com/coder/serpent.HostPort":
			// Custom marshal json to be a string
			return TypescriptType{ValueType: "string"}, nil
		case "github.com/coder/serpent.StringArray":
			return TypescriptType{ValueType: "string[]"}, nil
		case "github.com/coder/serpent.String":
			return TypescriptType{ValueType: "string"}, nil
		case "github.com/coder/serpent.YAMLConfigPath":
			return TypescriptType{ValueType: "string"}, nil
		case "github.com/coder/serpent.Strings":
			return TypescriptType{ValueType: "string[]"}, nil
		case "github.com/coder/serpent.Int64":
			return TypescriptType{ValueType: "number"}, nil
		case "github.com/coder/serpent.Bool":
			return TypescriptType{ValueType: "boolean"}, nil
		case "github.com/coder/serpent.Duration":
			return TypescriptType{ValueType: "number"}, nil
		case "net/url.URL":
			return TypescriptType{ValueType: "string"}, nil
		case "time.Time":
			// We really should come up with a standard for time.
			return TypescriptType{ValueType: "string"}, nil
		case "time.Duration":
			return TypescriptType{ValueType: "number"}, nil
		case "database/sql.NullTime":
			return TypescriptType{ValueType: "string", Optional: true}, nil
		case "github.com/coder/coder/v2/codersdk.NullTime":
			return TypescriptType{ValueType: "string", Optional: true}, nil
		case "github.com/google/uuid.NullUUID":
			return TypescriptType{ValueType: "string", Optional: true}, nil
		case "github.com/google/uuid.UUID":
			return TypescriptType{ValueType: "string"}, nil
		case "encoding/json.RawMessage":
			return TypescriptType{ValueType: "Record<string, string>"}, nil
		case "github.com/coder/serpent.URL":
			return TypescriptType{ValueType: "string"}, nil
		// XXX: For some reason, the type generator generates these as `any`
		//      so explicitly specifying the correct generic TS type.
		case "github.com/coder/coder/v2/codersdk.RegionsResponse[github.com/coder/coder/v2/codersdk.WorkspaceProxy]":
			return TypescriptType{ValueType: "RegionsResponse<WorkspaceProxy>"}, nil
		case "github.com/coder/coder/v2/coderd/healthcheck/health.Message":
			return TypescriptType{ValueType: "HealthMessage"}, nil
		case "github.com/coder/coder/v2/coderd/healthcheck/health.Severity":
			return TypescriptType{ValueType: "HealthSeverity"}, nil
		case "github.com/coder/coder/v2/healthsdk.HealthSection":
			return TypescriptType{ValueType: "HealthSection"}, nil
		case "github.com/coder/coder/v2/codersdk.ProvisionerDaemon":
			return TypescriptType{ValueType: "ProvisionerDaemon"}, nil
		}

		// Some hard codes are a bit trickier.
		//nolint:gocritic,revive // I prefer the switch for extensibility later.
		switch {
		// Struct is a generic, so the type has generic constraints in the string.
		case regexp.MustCompile(`github\.com/coder/serpent.Struct\[.*\]`).MatchString(n.String()):
			// The marshal json just marshals the underlying value.
			str, ok := ty.Underlying().(*types.Struct)
			if ok {
				return g.typescriptType(str.Field(0).Type())
			}
		}

		// Then see if the type is defined elsewhere. If it is, we can just
		// put the objName as it will be defined in the typescript codeblock
		// we generate.
		objName := objName(n.Obj())
		genericName := ""
		genericTypes := make(map[string]string)

		obj, objGen, local := g.lookupNamedReference(n)
		if obj != nil {
			if g.onlyOptIn && !slices.Contains(g.allowList, n.Obj().Name()) {
				// This is kludgy, but if we are an external package,
				// we need to also include dependencies. There is no
				// good way to return all extra types we need to include,
				// so just add them to the allow list and hope the caller notices
				// the slice grew...
				g.allowList = append(g.allowList, n.Obj().Name())
			}
			if !local {
				objGen.allowList = append(objGen.allowList, n.Obj().Name())
				g.log.Debug(context.Background(), "found external type",
					"name", objName,
					"ext_pkg", objGen.pkg.String(),
				)
			}
			// Sweet! Using other typescript types as fields. This could be an
			// enum or another struct
			if args := n.TypeArgs(); args != nil && args.Len() > 0 {
				genericConstraints := make([]string, 0, args.Len())
				genericNames := make([]string, 0, args.Len())
				for i := 0; i < args.Len(); i++ {
					genType, err := g.typescriptType(args.At(i))
					if err != nil {
						return TypescriptType{}, xerrors.Errorf("generic field %q<%q>: %w", objName, args.At(i).String(), err)
					}

					if param, ok := args.At(i).(*types.TypeParam); ok {
						// Using a generic defined by the parent.
						gname := param.Obj().Name()
						genericNames = append(genericNames, gname)
						genericTypes[gname] = genType.ValueType
					} else {
						// Defining a generic
						genericNames = append(genericNames, genType.ValueType)
					}

					genericConstraints = append(genericConstraints, genType.ValueType)
				}
				genericName = objName + fmt.Sprintf("<%s>", strings.Join(genericNames, ", "))
				objName += fmt.Sprintf("<%s>", strings.Join(genericConstraints, ", "))
			}

			cmt := ""
			return TypescriptType{
				GenericTypes:  genericTypes,
				GenericValue:  genericName,
				ValueType:     objName,
				AboveTypeLine: cmt,
			}, nil
		}

		// If it's a struct, just use the name of the struct type
		if _, ok := n.Underlying().(*types.Struct); ok {
			// External structs cannot be introspected, as we only parse the codersdk package.
			// You can handle your type manually in the switch list above, otherwise "any" will be used.
			// An easy way to fix this is to pull your external type into `codersdk` package, then it will
			// be known by the generator.
			return TypescriptType{ValueType: "any", AboveTypeLine: fmt.Sprintf("%s\n%s",
				indentedComment(fmt.Sprintf("Named type %q unknown, using \"any\"", n.String())),
				// Linter needs to be disabled here, or else it will complain about the "any" type.
				indentedComment("eslint-disable-next-line @typescript-eslint/no-explicit-any -- External type"),
			)}, nil
		}

		// Defer to the underlying type.
		ts, err := g.typescriptType(ty.Underlying())
		if err != nil {
			return TypescriptType{}, xerrors.Errorf("named underlying: %w", err)
		}
		if ts.AboveTypeLine == "" {
			// If no comment exists explaining where this type comes from, add one.
			ts.AboveTypeLine = indentedComment(fmt.Sprintf("This is likely an enum in an external package (%q)", n.String()))
		}
		return ts, nil
	case *types.Pointer:
		// Dereference pointers.
		pt := ty
		resp, err := g.typescriptType(pt.Elem())
		if err != nil {
			return TypescriptType{}, xerrors.Errorf("pointer: %w", err)
		}
		resp.Optional = true
		return resp, nil
	case *types.Interface:
		// only handle the empty interface (interface{}) for now
		intf := ty
		if intf.Empty() {
			// This field is 'interface{}'. We can't infer any type from 'interface{}'
			// so just use "any" as the type.
			return TypescriptType{
				ValueType: "any",
				AboveTypeLine: fmt.Sprintf("%s\n%s",
					indentedComment("Empty interface{} type, cannot resolve the type."),
					// Linter needs to be disabled here, or else it will complain about the "any" type.
					indentedComment("eslint-disable-next-line @typescript-eslint/no-explicit-any -- interface{}"),
				),
			}, nil
		}

		// Interfaces are difficult to determine the JSON type, so just return
		// an 'any'.
		return TypescriptType{
			ValueType:     "any",
			AboveTypeLine: indentedComment("eslint-disable-next-line @typescript-eslint/no-explicit-any -- Golang interface, unable to resolve type."),
			Optional:      false,
		}, nil
	case *types.TypeParam:
		_, ok := ty.Underlying().(*types.Interface)
		if !ok {
			// If it's not an interface, it is likely a usage of generics that
			// we have not hit yet. Feel free to add support for it.
			return TypescriptType{}, xerrors.New("type param must be an interface")
		}

		generic := ty.Constraint()
		// We don't mess with multiple packages, so just trim the package path
		// from the name.
		pkgPath := ty.Obj().Pkg().Path()
		name := strings.TrimPrefix(generic.String(), pkgPath+".")

		referenced := g.pkg.Types.Scope().Lookup(name)

		if referenced == nil {
			include, builtinString := g.isBuiltIn(name)
			if !include {
				// If we don't have the type constraint defined somewhere in the package,
				// then we have to resort to using any.
				return TypescriptType{
					GenericTypes: map[string]string{
						ty.Obj().Name(): "any",
					},
					GenericValue:  ty.Obj().Name(),
					ValueType:     "any",
					AboveTypeLine: fmt.Sprintf("// %q is an external type, so we use any", name),
					Optional:      false,
				}, nil
			}
			// Include the builtin for this type to reference
			g.builtins[name] = builtinString
		}

		return TypescriptType{
			GenericTypes: map[string]string{
				ty.Obj().Name(): name,
			},
			GenericValue:  ty.Obj().Name(),
			ValueType:     name,
			AboveTypeLine: "",
			Optional:      false,
		}, nil
	}

	// These are all the other types we need to support.
	// time.Time, uuid, etc.
	return TypescriptType{}, xerrors.Errorf("unknown type: %s", ty.String())
}

func (g *Generator) lookupNamedReference(n *types.Named) (obj types.Object, generator *Generator, local bool) {
	pkgName := n.Obj().Pkg().Name()

	if obj := g.pkg.Types.Scope().Lookup(n.Obj().Name()); g.pkg.Name == pkgName && obj != nil {
		return obj, g, true
	}

	for _, ext := range g.externals {
		if obj := ext.pkg.Types.Scope().Lookup(n.Obj().Name()); ext.pkg.Name == pkgName && obj != nil {
			return obj, ext, false
		}
	}

	return nil, nil, false
}

// isBuiltIn returns the string for a builtin type that we want to support
// if the name is a reserved builtin type. This is for types like 'comparable'.
// These types are not implemented in golang, so we just have to hardcode it.
func (Generator) isBuiltIn(name string) (bool, string) {
	// Note: @emyrk If we use constraints like Ordered, we can pull those
	// dynamically from their respective packages. This is a method on Generator
	// so if someone wants to implement that, they can find the respective package
	// and type.
	switch name {
	case "comparable":
		// To be complete, we include "any". Kinda sucks :(
		return true, "export type comparable = boolean | number | string | any"
	case "any":
		// This is supported in typescript, we don't need to write anything
		return true, ""
	default:
		return false, ""
	}
}

func indentedComment(comment string) string {
	return fmt.Sprintf("%s// %s", indent, comment)
}
