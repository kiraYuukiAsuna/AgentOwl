"use client";
import { Button } from "@/components/ui/button";
import { CircleAlert } from "lucide-react";
export default function ErrorPage({
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-4 px-6 py-24 text-center">
      <CircleAlert className="size-8 text-amber-600" />
      <h1 className="text-xl font-semibold">页面加载失败</h1>
      <Button onClick={reset}>重试</Button>
    </div>
  );
}
