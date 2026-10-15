// The Rock Media Center mark: a rock outcrop (Cobbling Rock) with sound bars and a play notch
// cut through it. Same path as web/public/icon.svg, framed tight on the mark; drawn in
// currentColor so it takes the theme's colour.
export function FleetMark({ className }: { className?: string }) {
  return (
    <svg viewBox="122 219 552 552" fill="none" className={className} aria-hidden>
      <path
        fill="currentColor"
        fillRule="evenodd"
        d="M132 676 L184 571 L222 549 L256 453 L300 422 L324 334 L364 295 L430 325 L452 365 L516 403 L541 482 L580 508 L616 614 L644 637 L664 676 Z M270 511 L270 586 L296 586 L296 511 Z M321 470 L321 627 L348 627 L348 470 Z M372 427 L372 627 L429 563 L429 500 Z M453 470 L453 627 L480 627 L480 470 Z M504 511 L504 586 L530 586 L530 511 Z"
      />
    </svg>
  );
}
