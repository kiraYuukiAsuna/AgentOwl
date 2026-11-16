import * as React from "react";
import { Slot } from "radix-ui";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";
const variants = cva(
  "inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-lg border border-transparent text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:pointer-events-none disabled:opacity-45 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default:
          "bg-primary text-primary-foreground hover:bg-primary/90",
        outline: "border border-border bg-card hover:bg-muted",
        ghost: "hover:bg-muted text-muted-foreground hover:text-foreground",
        destructive:
          "bg-red-50 text-red-700 hover:bg-red-100 border border-red-200",
      },
      size: {
        default: "h-8 px-3",
        sm: "h-7 px-2.5 text-[0.8rem]",
        icon: "size-8",
        lg: "h-10 px-4",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);
export function Button({
  className,
  variant,
  size,
  asChild,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> &
  VariantProps<typeof variants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot.Root : "button";
  return (
    <Comp className={cn(variants({ variant, size, className }))} {...props} />
  );
}
