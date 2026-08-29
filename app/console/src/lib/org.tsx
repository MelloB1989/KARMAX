import { createContext, useContext, useEffect, useState } from "react";
import { getOrganisation } from "@/api/organisation";
import type { OrgProfile } from "@/api/types";

// The organisation's own name, fetched once and shown wherever the console
// refers to "you".
//
// It used to say "Acme Inc" in the top bar — a fixture that shipped. A console
// that greets an operator with somebody else's company name is telling them,
// correctly, that nothing on the screen is really about them.

type OrgState = {
  org: OrgProfile | null;
  /** The name to show, falling back to the domain and then to nothing. */
  label: string;
  /** A short form for tight spaces: the domain's first label, or the name. */
  short: string;
  reload: () => void;
};

const Ctx = createContext<OrgState>({ org: null, label: "", short: "", reload: () => {} });

export function OrgProvider({ children }: { children: React.ReactNode }) {
  const [org, setOrg] = useState<OrgProfile | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let live = true;
    // A failure here is not worth surfacing: the console works without a name,
    // and an error banner about branding would be louder than the problem.
    getOrganisation()
      .then((o) => live && setOrg(o))
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [nonce]);

  const name = (org?.name ?? "").trim();
  const domain = (org?.domain ?? "").trim();
  const label = name || domain;
  const short = domain ? domain.split(".")[0] : name;

  return (
    <Ctx.Provider value={{ org, label, short, reload: () => setNonce((n) => n + 1) }}>
      {children}
    </Ctx.Provider>
  );
}

export function useOrg(): OrgState {
  return useContext(Ctx);
}
