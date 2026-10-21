import { LayoutTemplate } from "lucide-react";

/**
 * Small provenance line for objects created from an app template. Shows
 * sparingly (design: store generously, show sparingly) — a static badge, not a
 * live link: drift after the user edits the object is expected.
 */
export function ProvenanceNote({
  sourceKind,
  sourceRef,
  className,
}: {
  sourceKind: string | null;
  sourceRef: string | null;
  className?: string;
}) {
  if (sourceKind !== "template" || !sourceRef) return null;
  return (
    <span
      className={className}
      title={`Created from template ${sourceRef}`}
    >
      <LayoutTemplate
        aria-hidden="true"
        className="mr-1 inline-block size-3.5 align-[-2px]"
      />
      From template {sourceRef}
    </span>
  );
}
