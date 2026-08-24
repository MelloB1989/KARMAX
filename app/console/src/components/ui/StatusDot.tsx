import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import type { ConnectorStatus, SandboxRunStatus } from "@/api/types";

// The ONE semantic-colour vocabulary in the console: healthy/degraded/failed,
// used only for machine health (connectors, sandbox runs, integrations) —
// never reused for case workflow state or the brand accent.
export type Health = "healthy" | "degraded" | "failed" | "unknown";

const dotColor: Record<Health, string> = {
  healthy: "bg-healthy",
  degraded: "bg-degraded",
  failed: "bg-failed",
  unknown: "bg-unknown",
};

const textColor: Record<Health, string> = {
  healthy: "text-healthy",
  degraded: "text-degraded",
  failed: "text-failed",
  unknown: "text-unknown",
};

export function connectorHealth(status: ConnectorStatus): Health {
  if (status === "not_configured") return "unknown";
  return status;
}

export function sandboxHealth(status: SandboxRunStatus): Health {
  switch (status) {
    case "running":
    case "starting":
      return "healthy";
    case "exited":
      return "healthy";
    case "failed":
      return "failed";
    case "gone":
      return "degraded";
  }
}

export function StatusDot({ health, pulse = false }: { health: Health; pulse?: boolean }) {
  return (
    <span
      className={cn(
        "inline-block h-2 w-2 shrink-0 rounded-full",
        dotColor[health],
        pulse && "animate-pulse-soft",
      )}
      aria-hidden
    />
  );
}

export function StatusLabel({
  health, pulse = false, children,
}: { health: Health; pulse?: boolean; children: ReactNode }) {
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-sm font-medium", textColor[health])}>
      <StatusDot health={health} pulse={pulse} />
      {children}
    </span>
  );
}
