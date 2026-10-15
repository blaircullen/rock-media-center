import type { ReactNode } from "react";

// The requester shell's tab icons: line icons in the same stroke style as the bell and
// the menu button (24 viewBox, stroke 2, currentColor). Kept apart from icons.tsx, the
// staff sidebar's set, so a requester's first load carries only these four.

export type ShellIconName = "discover" | "requests" | "calendar" | "me";

const PATHS: Record<ShellIconName, ReactNode> = {
  // A compass.
  discover: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M15.5 8.5l-2 5-5 2 2-5z" />
    </>
  ),
  // A list with a tick: what you asked for.
  requests: (
    <>
      <path d="M9 6h11M9 12h11M9 18h11" />
      <path d="M4 6l1 1 2-2M4 12l1 1 2-2M4 18l1 1 2-2" />
    </>
  ),
  calendar: (
    <>
      <rect x="3.5" y="5" width="17" height="15.5" rx="2" />
      <path d="M3.5 10h17M8 3v4M16 3v4" />
    </>
  ),
  me: (
    <>
      <circle cx="12" cy="8" r="4" />
      <path d="M4.5 20.5a7.5 7.5 0 0 1 15 0" />
    </>
  ),
};

export function ShellIcon({ name, className }: { name: ShellIconName; className?: string }) {
  return (
    <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className={className} aria-hidden>
      {PATHS[name]}
    </svg>
  );
}
