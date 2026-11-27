/**
 * The T3 Code thread a session works in, "t3:" and its id's first eight
 * characters; with the link T3 Code gives the thread, choosing it opens the
 * thread (T3 Code's page offers its app or the web).
 */
export function T3ThreadTag({ id, url, className = "" }: { id: string; url?: string; className?: string }) {
  const label = `t3:${id.slice(0, 8)}`;
  if (!url) {
    return (
      <span className={className} title={`T3 Code thread ${id}`}>
        {label}
      </span>
    );
  }
  return (
    <a href={url} target="_blank" rel="noreferrer" className={`${className} hover:text-primary hover:underline`.trim()} title={`Open T3 Code thread ${id}`}>
      {label} ↗
    </a>
  );
}
