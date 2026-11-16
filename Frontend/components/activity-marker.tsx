import { cn } from "@/lib/utils";
import type { ActivityTone } from "@/lib/activity-labels";

export const activityToneClass: Record<ActivityTone, string> = {
  user: "bg-sky-50 text-sky-700 ring-sky-200",
  feedback: "bg-emerald-50 text-emerald-700 ring-emerald-200",
  system: "bg-amber-50 text-amber-800 ring-amber-200",
  platform: "bg-violet-50 text-violet-700 ring-violet-200",
  operation: "bg-muted text-muted-foreground ring-border",
};

export const activityDotClass: Record<ActivityTone, string> = {
  user: "bg-sky-400",
  feedback: "bg-emerald-500",
  system: "bg-amber-400",
  platform: "bg-violet-400",
  operation: "bg-primary/35",
};

export function ActivityMarker({
  label,
  tone,
  className,
}: {
  label: string;
  tone: ActivityTone;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-[11px] font-medium ring-1",
        activityToneClass[tone],
        className,
      )}
    >
      {label}
    </span>
  );
}
