"use client";
import { Dialog as Primitive } from "radix-ui";
import { X } from "lucide-react";
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
}: {
  open: boolean;
  onOpenChange: (value: boolean) => void;
  title: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <Primitive.Root open={open} onOpenChange={onOpenChange}>
      <Primitive.Portal>
        <Primitive.Overlay className="fixed inset-0 z-50 bg-slate-950/30" />
        <Primitive.Content {...(description ? {} : { "aria-describedby": undefined })} className="fixed top-1/2 left-1/2 z-50 max-h-[88dvh] w-[calc(100%-2rem)] max-w-xl -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-xl border border-border bg-card p-6 shadow-xl focus:outline-none">
          <Primitive.Title className="pr-8 text-lg font-semibold tracking-tight">
            {title}
          </Primitive.Title>
          {description && (
            <Primitive.Description className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
              {description}
            </Primitive.Description>
          )}
          <Primitive.Close
            className="absolute top-4 right-4 flex size-8 items-center justify-center rounded-md text-muted-foreground hover:bg-muted hover:text-foreground"
            aria-label="关闭"
          >
            <X className="size-4" />
          </Primitive.Close>
          <div className="mt-6">{children}</div>
        </Primitive.Content>
      </Primitive.Portal>
    </Primitive.Root>
  );
}
