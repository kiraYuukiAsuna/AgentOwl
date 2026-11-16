import * as React from "react";
import { cn } from "@/lib/utils";
export function Input({
  className,
  ...props
}: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cn(
        "h-9 w-full rounded-lg border border-border bg-card px-3 text-sm outline-none transition-colors focus:border-ring focus:ring-2 focus:ring-ring/15 placeholder:text-muted-foreground/75 disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}
export function Textarea({
  className,
  ...props
}: React.TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return (
    <textarea
      className={cn(
        "min-h-28 w-full resize-y rounded-lg border border-border bg-card p-3 text-sm leading-relaxed outline-none transition-colors focus:border-ring focus:ring-2 focus:ring-ring/15 placeholder:text-muted-foreground/75",
        className,
      )}
      {...props}
    />
  );
}
export function Select({
  className,
  children,
  ...props
}: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={cn(
        "h-9 w-full rounded-lg border border-border bg-card px-3 pr-8 text-sm outline-none focus:border-ring focus:ring-2 focus:ring-ring/15",
        className,
      )}
      {...props}
    >
      {children}
    </select>
  );
}
export function Field({
  label,
  hint,
  group = false,
  children,
}: {
  label: string;
  hint?: string;
  group?: boolean;
  children: React.ReactNode;
}) {
  const Container = group ? "fieldset" : "label";
  const Label = group ? "legend" : "span";
  return (
    <Container className="block space-y-1.5">
      <Label className="text-sm font-medium">{label}</Label>
      {children}
      {hint && (
        <span className="block text-xs leading-relaxed text-muted-foreground">
          {hint}
        </span>
      )}
    </Container>
  );
}
