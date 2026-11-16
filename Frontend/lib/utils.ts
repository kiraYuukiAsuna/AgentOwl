import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
export function cn(...values: ClassValue[]) {
  return twMerge(clsx(values));
}
export function dateLabel(value?: string | Date | null) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.valueOf())
    ? "—"
    : new Intl.DateTimeFormat("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      }).format(date);
}
export function shortId(value?: string | null) {
  return value ? value.slice(0, 8).toUpperCase() : "—";
}
export function objectData(
  value?: { additionalData?: Record<string, unknown> } | null,
) {
  return value?.additionalData ?? {};
}
