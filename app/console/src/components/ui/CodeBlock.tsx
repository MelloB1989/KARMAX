import { cn } from "@/lib/utils";

// A tiny regex-based YAML tint — not a real tokenizer, just enough contrast
// between keys, strings and comments that a recipe reads at a glance. Kept
// dependency-free on purpose: this is an operator tool, not an IDE.
function highlightYaml(src: string): string {
  const esc = (s: string) =>
    s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return esc(src)
    .split("\n")
    .map((line) => {
      if (/^\s*#/.test(line)) return `<span class="text-fg-subtle italic">${line}</span>`;
      const m = line.match(/^(\s*(?:-\s+)?)([A-Za-z0-9_.]+)(:)(.*)$/);
      if (m) {
        const [, indent, key, colon, rest] = m;
        return `${indent}<span class="text-brand-700 light:text-brand-400 font-semibold">${key}</span><span class="text-fg-subtle">${colon}</span><span class="text-fg">${esc_rest(rest)}</span>`;
      }
      return `<span class="text-fg">${line}</span>`;
    })
    .join("\n");

  function esc_rest(rest: string): string {
    const str = rest.match(/^(\s*)("(?:[^"\\]|\\.)*")(.*)$/);
    if (str) return `${str[1]}<span class="text-healthy">${str[2]}</span>${str[3]}`;
    return rest;
  }
}

export function CodeBlock({ code, className }: { code: string; className?: string }) {
  return (
    <pre
      className={cn(
        "overflow-x-auto rounded-[var(--radius-card)] border border-border bg-bg-soft p-4 font-mono text-[13px] leading-relaxed text-fg",
        className,
      )}
    >
      <code dangerouslySetInnerHTML={{ __html: highlightYaml(code) }} />
    </pre>
  );
}
