import { NavLink } from "react-router-dom";
import {
  KanbanSquare, Bot, Workflow, CircleCheck, ScrollText, Plug, Building2, Users, Webhook,
  Settings as SettingsIcon,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useOrg } from "@/lib/org";

// Grouped, because a flat list of seven makes the operator read all seven every
// time. Work is what you are doing now; Fleet is what does it; Record is what
// it did.
const GROUPS = [
  {
    label: "Work",
    items: [
      { to: "/cases", label: "Cases", icon: KanbanSquare },
      { to: "/approvals", label: "Approvals", icon: CircleCheck },
    ],
  },
  {
    label: "Fleet",
    items: [
      { to: "/agents", label: "Agents", icon: Bot },
      { to: "/recipes", label: "Recipes", icon: Workflow },
      { to: "/connectors", label: "Connectors", icon: Plug },
      { to: "/webhooks", label: "Webhooks", icon: Webhook },
    ],
  },
  {
    label: "Record",
    items: [
      { to: "/audit", label: "Audit", icon: ScrollText },
    ],
  },
  // Who the agents work for, and who may tell them to. Separated from Record
  // because these change rarely and are read even more rarely — they should not
  // sit next to the pages an operator opens every day.
  {
    label: "Organisation",
    items: [
      { to: "/organisation", label: "Organisation", icon: Building2 },
      { to: "/users", label: "Users", icon: Users },
      { to: "/settings", label: "Settings", icon: SettingsIcon },
    ],
  },
];

export function Sidebar() {
  const { short } = useOrg();

  return (
    <aside className="flex h-full w-52 shrink-0 flex-col border-r border-border bg-bg-soft">
      <div className="flex h-13 items-center gap-2.5 border-b border-border px-4">
        <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" className="text-accent">
          <path d="M4 7h7M4 12h16M13 17h7" />
          <circle cx="15" cy="7" r="2.4" />
          <circle cx="9" cy="17" r="2.4" />
        </svg>
        <span className="text-[13.5px] font-semibold tracking-tight">oCrew</span>
        {/* The organisation, not a fixture. Absent until somebody names it —
            an empty space is honest, "acme" was not. */}
        {short && (
          <span className="ml-auto truncate rounded-[3px] border border-border px-1.5 font-mono text-[9.5px] text-fg-subtle">
            {short}
          </span>
        )}
      </div>

      <nav className="flex-1 overflow-y-auto px-2.5 py-3">
        {GROUPS.map((group) => (
          <div key={group.label} className="mb-4 last:mb-0">
            <div className="px-1.5 pb-1.5 text-label font-semibold uppercase text-fg-subtle">
              {group.label}
            </div>
            <div className="space-y-px">
              {group.items.map(({ to, label, icon: Icon }) => (
                <NavLink
                  key={to}
                  to={to}
                  className={({ isActive }) =>
                    cn(
                      "flex items-center gap-2.5 rounded-[5px] px-2 py-1.5 text-[13px] transition-colors",
                      isActive
                        ? "bg-surface-2 font-medium text-fg"
                        : "text-fg-muted hover:bg-surface hover:text-fg",
                    )
                  }
                >
                  <Icon className="h-[15px] w-[15px]" strokeWidth={1.7} />
                  {label}
                </NavLink>
              ))}
            </div>
          </div>
        ))}
      </nav>

      <div className="flex items-center gap-2 border-t border-border px-3.5 py-3">
        <span className="h-1.5 w-1.5 rounded-full bg-healthy" />
        <span className="font-mono text-[10.5px] text-fg-subtle">daemon · 6d 04h</span>
      </div>
    </aside>
  );
}
