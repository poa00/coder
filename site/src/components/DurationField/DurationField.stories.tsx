import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import { DurationField } from "./DurationField";

const meta: Meta<typeof DurationField> = {
  title: "components/DurationField",
  component: DurationField,
  args: {
    label: "Duration",
  },
  render: function RenderComponent(args) {
    const [value, setValue] = useState<number>(args.value);
    return (
      <DurationField
        {...args}
        value={value}
        onChange={(value) => setValue(value)}
      />
    );
  },
};

export default meta;
type Story = StoryObj<typeof DurationField>;

export const Hours: Story = {
  args: {
    value: hoursToMs(16),
  },
};

export const Days: Story = {
  args: {
    value: daysToMs(2),
  },
};

function hoursToMs(hours: number): number {
  return hours * 60 * 60 * 1000;
}

function daysToMs(days: number): number {
  return days * 24 * 60 * 60 * 1000;
}
