export function hasRole(
  user: { role?: string | null; roles?: string[] | null } | undefined,
  ...allowed: string[]
) {
  const roles = new Set([user?.role, ...(user?.roles ?? [])]);
  return roles.has("admin") || allowed.some((role) => roles.has(role));
}

export function safeReturnPath(value: string | null): string {
  if (!value?.startsWith("/")) return "/";
  try {
    const url = new URL(value, "https://workspace.invalid");
    return url.origin === "https://workspace.invalid"
      ? `${url.pathname}${url.search}${url.hash}`
      : "/";
  } catch {
    return "/";
  }
}
