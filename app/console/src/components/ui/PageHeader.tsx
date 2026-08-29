import type { ReactNode } from "react";

/**
 * How every page opens: the title, and the register line under it.
 *
 * The register is the machine's own voice — how many of what, and in what
 * state — set in mono and read before anything else. It exists because the
 * question an operator arrives with is never "what is this page", it is "is
 * anything wrong", and a title alone cannot answer that.
 */
export function PageHeader({
  title,
  register,
  children,
}: {
  title: string;
  /** Short factual clauses. Joined with a separator, never a sentence. */
  register?: (string | false | null | undefined)[];
  /** Actions, kept to the right and quiet. */
  children?: ReactNode;
}) {
  const parts = (register ?? []).filter(Boolean) as string[];

  return (
    <div className="mb-6 flex items-end justify-between gap-6 border-b border-border pb-4">
      <div className="min-w-0">
        <h1 className="text-h1 text-fg">{title}</h1>
        {parts.length > 0 && (
          <p className="mt-1.5 font-mono text-register text-fg-subtle">
            {parts.map((p, i) => (
              <span key={i}>
                {i > 0 && <span className="mx-2 text-border-strong">/</span>}
                {p}
              </span>
            ))}
          </p>
        )}
      </div>
      {children && <div className="flex shrink-0 items-center gap-2">{children}</div>}
    </div>
  );
}
