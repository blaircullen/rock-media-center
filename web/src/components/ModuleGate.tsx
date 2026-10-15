import type { ReactNode } from "react";
import { Navigate } from "react-router-dom";
import { useMe } from "../lib/me";

export type Module = "music";

// moduleOn reports whether an admin has the module switched on.
export function moduleOn(module: Module, s: { musicEnabled: boolean }): boolean {
  return module === "music" && s.musicEnabled;
}

// ModuleGate guards the pages of a module an admin can switch off. The routes stay in the
// router either way: switching Music off in Settings sends anyone on a Music page home
// straight away, without rebuilding the router (which would throw away Back history).
export function ModuleGate({ module, home, children }: { module: Module; home: string; children: ReactNode }) {
  const me = useMe();
  if (!moduleOn(module, me)) return <Navigate to={home} replace />;
  return <>{children}</>;
}
