"use client";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect } from "react";
import { Command } from "cmdk";
import { Dialog } from "radix-ui";
import {
  Activity,
  ListTodo,
  BookOpen,
  ChevronRight,
  Code2,
  Inbox,
  LayoutDashboard,
  LogOut,
  Menu,
  Plus,
  Search,
  Settings2,
  ShieldCheck,
  UserRound,
  X,
} from "lucide-react";
import { useUI } from "@/lib/stores/ui";
import { hasRole } from "@/lib/access";
import { useMe, api } from "@/lib/api/client";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { label, Loading, ErrorState, reportError } from "@/components/common";

const navigation = [
  { href: "/", title: "概览", icon: LayoutDashboard },
  { href: "/cases", title: "问题", icon: Inbox },
  { href: "/approvals", title: "审批", icon: ShieldCheck },
  { href: "/development", title: "开发任务", icon: Code2 },
  { href: "/knowledge", title: "知识库", icon: BookOpen },
];
const management = [
  { href: "/settings/connections", title: "设置", icon: Settings2 },
  { href: "/audit", title: "操作记录", icon: Activity },
  { href: "/jobs", title: "后台任务", icon: ListTodo },
];
const account = { href: "/profile", title: "个人中心", icon: UserRound };
export function OwlMark({ className = "size-8" }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 36 36" fill="none" aria-hidden="true">
      <rect width="36" height="36" rx="8" fill="currentColor" />
      <path d="M9 11.5 13.5 14h9L27 11.5v9a9 9 0 0 1-18 0v-9Z" fill="white" />
      <circle cx="14" cy="19" r="3.5" fill="currentColor" />
      <circle cx="22" cy="19" r="3.5" fill="currentColor" />
      <circle cx="14.7" cy="18.5" r="1.1" fill="white" />
      <circle cx="22.7" cy="18.5" r="1.1" fill="white" />
      <path d="m16.5 23 1.5 2 1.5-2" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const { data: me, error, isLoading, mutate } = useMe();
  const { commandOpen, setCommandOpen, mobileOpen, setMobileOpen } = useUI();
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setCommandOpen(!useUI.getState().commandOpen);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [setCommandOpen]);
  useEffect(() => {
    if (error?.status === 401)
      router.replace(`/login?next=${encodeURIComponent(pathname)}`);
  }, [error, pathname, router]);
  useEffect(() => {
    const desktop = window.matchMedia("(min-width: 48rem)");
    const closeOnDesktop = () => {
      if (desktop.matches) setMobileOpen(false);
    };
    closeOnDesktop();
    desktop.addEventListener("change", closeOnDesktop);
    return () => desktop.removeEventListener("change", closeOnDesktop);
  }, [setMobileOpen]);
  if (isLoading || error?.status === 401)
    return <div className="flex min-h-screen items-center justify-center"><Loading /></div>;
  if (error || !me)
    return <div className="mx-auto max-w-xl pt-32"><ErrorState error={error} retry={() => void mutate()} /></div>;

  const managementItems = management.filter(
    (item) => item.href !== "/jobs" || hasRole(me, "admin"),
  );
  const isActive = (href: string) => href === "/" ? pathname === href : pathname.startsWith(href.split("/").slice(0, 2).join("/"));
  const section = [...navigation, ...managementItems, account].find((item) => isActive(item.href))?.title ?? "工作台";
  const go = (href: string) => {
    router.push(href);
    setCommandOpen(false);
    setMobileOpen(false);
  };
  const sidebar = (
    <>
      <div className="flex-1 overflow-y-auto px-3 py-6">
        {[
          { title: "工作台", items: navigation },
          { title: "管理", items: managementItems },
        ].map((group) => (
          <nav key={group.title} className="mb-7 space-y-1" aria-label={group.title}>
            <p className="mb-2 px-2.5 text-[11px] font-semibold tracking-[0.14em] text-muted-foreground">{group.title}</p>
            {group.items.map(({ href, title, icon: Icon }) => (
              <Link
                key={href}
                href={href}
                aria-current={isActive(href) ? "page" : undefined}
                onClick={() => setMobileOpen(false)}
                className={cn(
                  "flex h-9 items-center gap-2.5 rounded-xl px-2.5 text-sm font-medium transition-colors hover:bg-accent hover:text-accent-foreground",
                  isActive(href)
                    ? "bg-primary/8 font-medium text-primary shadow-[inset_3px_0_0_var(--primary)]"
                    : "text-sidebar-foreground/72",
                )}
              >
                <Icon className={cn("size-4 text-muted-foreground", isActive(href) && "text-primary")} aria-hidden />
                {title}
              </Link>
            ))}
          </nav>
        ))}
      </div>
      <div className="flex items-center gap-2.5 border-t border-border px-4 py-4">
        <Link href="/profile" onClick={() => setMobileOpen(false)} className="flex min-w-0 flex-1 items-center gap-2.5 rounded-lg p-1.5 hover:bg-muted" aria-label="个人中心">
          <div className="flex size-8 shrink-0 items-center justify-center rounded-full border border-border bg-card text-xs font-medium text-muted-foreground">
            {(me.name ?? me.email ?? "U").slice(0, 1).toUpperCase()}
          </div>
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-medium">{me.name ?? me.email}</p>
            <p className="mt-0.5 text-xs text-muted-foreground">{label(me.role)}</p>
          </div>
        </Link>
        <Button
          variant="ghost"
          size="icon"
          aria-label="退出登录"
          onClick={async () => {
            try {
              await api.logout();
              await mutate(undefined, { revalidate: false });
              router.replace("/login");
            } catch (e) { reportError(e); }
          }}
        >
          <LogOut className="size-4" />
        </Button>
      </div>
    </>
  );
  return (
    <Dialog.Root open={mobileOpen} onOpenChange={setMobileOpen}>
    <div className="min-h-dvh">
      <a href="#main-content" className="fixed top-2 left-2 z-[70] -translate-y-16 rounded-lg bg-primary px-3 py-2 text-sm text-primary-foreground focus:translate-y-0">跳到主要内容</a>
      <header className="sticky top-0 z-30 flex h-16 min-w-0 items-center gap-4 border-b border-border bg-card/95 px-4 backdrop-blur-md md:px-6">
        <div className="flex shrink-0 items-center gap-3 md:w-[176px]">
          <Dialog.Trigger asChild><Button className="md:hidden" variant="ghost" size="icon" aria-label="打开导航"><Menu className="size-5" /></Button></Dialog.Trigger>
          <Link href="/" className="flex items-center gap-2.5" aria-label="AgentOwl 首页">
            <OwlMark className="size-8 text-primary" />
            <span className="text-[15px] font-semibold tracking-tight">AgentOwl</span>
          </Link>
        </div>
        <div className="hidden min-w-0 items-center gap-3 text-sm md:flex">
          <span className="shrink-0 text-muted-foreground">AgentOwl</span>
          <ChevronRight className="size-3.5 shrink-0 text-muted-foreground/50" aria-hidden />
          <span className="truncate font-medium">{section}</span>
        </div>
        <div className="ml-auto flex items-center gap-2 sm:gap-3">
          <button onClick={() => setCommandOpen(true)} className="flex h-8 items-center gap-2 rounded-lg border border-border bg-background px-2.5 text-xs text-muted-foreground hover:bg-muted" aria-label="快速跳转">
            <Search className="size-3.5" aria-hidden />
            <span className="hidden sm:block">快速跳转</span>
            <kbd className="ml-6 hidden rounded border border-border bg-card px-1 py-0.5 font-sans text-[10px] sm:block">⌘ K</kbd>
          </button>
          {hasRole(me, "developer", "oncall") && pathname !== "/" && !pathname.startsWith("/cases") && (
            <Button asChild className="hidden sm:inline-flex"><Link href="/cases?new=1"><Plus className="size-3.5" />新建问题</Link></Button>
          )}
        </div>
      </header>
      <div className="flex min-h-[calc(100dvh-4rem)] min-w-0">
        <aside className="sticky top-16 hidden h-[calc(100dvh-4rem)] w-56 shrink-0 flex-col border-r border-border bg-sidebar/70 md:flex">{sidebar}</aside>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 z-50 bg-slate-950/30 md:hidden" />
          <Dialog.Content className="fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r border-border bg-sidebar focus:outline-none md:hidden">
            <div className="flex h-16 shrink-0 items-center justify-between border-b border-border px-5">
              <Dialog.Title className="text-sm font-semibold">AgentOwl</Dialog.Title>
              <Dialog.Description className="sr-only">选择页面</Dialog.Description>
              <Dialog.Close asChild><Button variant="ghost" size="icon" aria-label="关闭导航"><X className="size-4" /></Button></Dialog.Close>
            </div>
            {sidebar}
          </Dialog.Content>
        </Dialog.Portal>
        <div className="flex min-w-0 flex-1 flex-col">
          <main id="main-content" tabIndex={-1} className="mx-auto w-full max-w-[1440px] flex-1 px-4 py-7 outline-none sm:px-6 lg:px-8 lg:py-8">{children}</main>
          <footer className="mx-4 flex justify-between border-t border-border py-4 text-[11px] text-muted-foreground sm:mx-6 lg:mx-8"><span>AgentOwl</span><span>AgentOwl</span></footer>
        </div>
      </div>
      <Command.Dialog open={commandOpen} onOpenChange={setCommandOpen} label="快速跳转" className="fixed top-[20%] left-1/2 z-[60] w-[calc(100%-2rem)] max-w-lg -translate-x-1/2 overflow-hidden rounded-xl border border-border bg-card shadow-xl">
        <div className="flex items-center gap-3 border-b border-border px-4">
          <Search className="size-4 text-muted-foreground" aria-hidden />
          <Command.Input placeholder="搜索页面…" className="h-12 min-w-0 flex-1 bg-transparent text-sm outline-none focus:outline-none focus-visible:outline-none" />
          <button onClick={() => setCommandOpen(false)} aria-label="关闭搜索" className="rounded p-1 text-muted-foreground hover:bg-muted"><X className="size-4" /></button>
        </div>
        <Command.List className="max-h-80 overflow-y-auto p-2">
          <Command.Empty className="p-6 text-center text-sm text-muted-foreground">没有匹配的页面</Command.Empty>
          {[...navigation, ...managementItems, account].map(({ href, title, icon: Icon }) => (
            <Command.Item key={href} onSelect={() => go(href)} className="flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2.5 text-sm aria-selected:bg-accent aria-selected:text-accent-foreground"><Icon className="size-4" aria-hidden />{title}</Command.Item>
          ))}
          {hasRole(me, "developer", "oncall") && (
            <Command.Item onSelect={() => go("/cases?new=1")} className="flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2.5 text-sm aria-selected:bg-accent aria-selected:text-accent-foreground"><Plus className="size-4" />新建问题</Command.Item>
          )}
        </Command.List>
      </Command.Dialog>
    </div>
    </Dialog.Root>
  );
}
