export function downloadBinary(content: ArrayBuffer, filename: string) {
  const url = URL.createObjectURL(
    new Blob([content], { type: "application/octet-stream" }),
  );
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
