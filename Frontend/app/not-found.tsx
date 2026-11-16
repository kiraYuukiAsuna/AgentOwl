import Link from "next/link";
import { Button } from "@/components/ui/button";
export default function NotFound() {
  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center gap-4">
      <p className="font-mono text-sm text-muted-foreground">404</p>
      <h1 className="text-xl font-semibold">页面不存在</h1>
      <Button asChild variant="outline">
        <Link href="/">返回概览</Link>
      </Button>
    </div>
  );
}
