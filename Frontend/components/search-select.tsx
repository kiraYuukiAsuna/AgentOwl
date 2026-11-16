"use client";
import { useId, useState } from "react";
import { Popover } from "radix-ui";
import { Command } from "cmdk";
import { Check, ChevronsUpDown, Search } from "lucide-react";
import { cn } from "@/lib/utils";
export function SearchSelect({
  value,
  onChange,
  options,
  placeholder = "请选择",
  disabled = false,
}: {
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
  placeholder?: string;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const listId = useId();
  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
      <Popover.Trigger asChild>
        <button
          type="button"
          disabled={disabled}
          role="combobox"
          aria-expanded={open}
          aria-controls={listId}
          className="flex h-9 w-full items-center justify-between gap-2 rounded-lg border border-border bg-card px-3 text-left text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 disabled:opacity-50"
        >
          <span className={cn("truncate", !value && "text-muted-foreground")}>
            {options.find((o) => o.value === value)?.label || placeholder}
          </span>
          <ChevronsUpDown className="size-3.5 shrink-0 text-muted-foreground" />
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          sideOffset={6}
          className="z-[60] w-[var(--radix-popover-trigger-width)] min-w-48 overflow-hidden rounded-lg border border-border bg-card shadow-lg"
        >
          <Command>
            <div className="flex items-center gap-2 border-b border-border px-3">
              <Search className="size-3.5 text-muted-foreground" />
              <Command.Input
                placeholder="搜索…"
                className="h-10 w-full text-sm outline-none focus:outline-none focus-visible:outline-none"
              />
            </div>
            <Command.List id={listId} className="max-h-60 overflow-auto p-1">
              <Command.Empty className="p-4 text-xs text-muted-foreground">
                无匹配选项
              </Command.Empty>
              {options.map((o) => (
                <Command.Item
                  key={o.value}
                  value={`${o.label} ${o.value}`}
                  onSelect={() => {
                    onChange(o.value);
                    setOpen(false);
                  }}
                  className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-2 text-sm aria-selected:bg-muted"
                >
                  <Check
                    className={cn("size-3.5", o.value !== value && "opacity-0")}
                  />
                  {o.label}
                </Command.Item>
              ))}
            </Command.List>
          </Command>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  );
}
