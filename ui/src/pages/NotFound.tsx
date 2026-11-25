import { Link } from "@tanstack/react-router";

export function NotFound() {
  return (
    <div className="py-24 text-center">
      <div className="display text-[64px] font-semibold italic">bare soil</div>
      <p className="mt-2 text-[13px] text-muted-foreground">Nothing grows at this address.</p>
      <Link to="/" className="mt-6 inline-block font-mono text-[12px] text-primary">
        ← projects
      </Link>
    </div>
  );
}
