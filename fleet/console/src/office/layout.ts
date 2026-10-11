export interface DeskPlace {
  id: string;
  x: number;
  z: number;
  rotY: number;
}

export const ROW_SPACING = 3.2;
export const PER_ROW = 4;
export const HEAD_Z = -6;

/** The orchestrator sits at the head; agents follow in centred rows of up to 4. */
export function deskLayout(agentIds: string[], orchestrator: string): DeskPlace[] {
  const out: DeskPlace[] = [{ id: orchestrator, x: 0, z: HEAD_Z, rotY: 0 }];
  const ids = agentIds.filter((id) => id !== orchestrator);
  for (let i = 0; i < ids.length; i += PER_ROW) {
    const row = ids.slice(i, i + PER_ROW);
    const z = -2 + (i / PER_ROW) * ROW_SPACING;
    row.forEach((id, k) => {
      out.push({ id, x: (k - (row.length - 1) / 2) * ROW_SPACING, z, rotY: 0 });
    });
  }
  return out;
}
