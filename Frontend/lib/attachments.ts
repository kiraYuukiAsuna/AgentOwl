import { z } from "zod";

const reference = z.object({
  key: z.string().min(1),
  type: z.enum(["image", "file"]),
  name: z.string().optional(),
});

export function attachmentReferences(value: unknown) {
  if (!Array.isArray(value)) return [];
  // Keep the server's array index even when a reference is unreadable.
  return value.map((entry, index) => {
    const parsed = reference.safeParse(entry);
    return {
      index,
      readable: parsed.success,
      key: parsed.success ? parsed.data.key : "",
      type: parsed.success ? parsed.data.type : "unknown",
      name: parsed.success
        ? parsed.data.name || `附件 ${index + 1}`
        : `无法解析的附件 ${index + 1}`,
    };
  });
}

export function attachmentFilename(name: string, index: number) {
  const filename =
    name.replace(/[\\/\u0000-\u001f\u007f]/g, "_").slice(0, 160) ||
    `attachment-${index + 1}`;
  return /\.(png|jpg|jpeg|gif|webp|pdf|txt|log|json|md|csv|yaml|yml)$/i.test(filename)
    ? filename
    : `${filename}.bin`;
}
