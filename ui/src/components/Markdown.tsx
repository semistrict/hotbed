import { Fragment } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

/**
 * Markdown agents write: GitHub's flavor, never raw HTML (react-markdown
 * leaves it out), styled to sit in the surrounding text's size and color.
 */
const INLINE: Components = {
  // A link opens on its own, not whatever it sits in: an option it describes, say.
  a: ({ node: _, ...p }) => <a {...p} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()} className="text-frost underline decoration-frost/40 underline-offset-2 hover:decoration-frost" />,
  code: ({ node: _, ...p }) => <code {...p} className="rounded bg-muted px-1 py-px font-mono text-[0.88em] text-foreground" />,
  strong: ({ node: _, ...p }) => <strong {...p} className="font-semibold text-foreground" />,
};

const BLOCK: Components = {
  ...INLINE,
  p: ({ node: _, ...p }) => <p {...p} className="my-1.5 first:mt-0 last:mb-0" />,
  ul: ({ node: _, ...p }) => <ul {...p} className="my-1.5 list-disc space-y-0.5 pl-5 first:mt-0 last:mb-0" />,
  ol: ({ node: _, ...p }) => <ol {...p} className="my-1.5 list-decimal space-y-0.5 pl-5 first:mt-0 last:mb-0" />,
  h1: ({ node: _, ...p }) => <h3 {...p} className="mt-3 mb-1 font-semibold text-foreground first:mt-0" />,
  h2: ({ node: _, ...p }) => <h3 {...p} className="mt-3 mb-1 font-semibold text-foreground first:mt-0" />,
  h3: ({ node: _, ...p }) => <h4 {...p} className="mt-2.5 mb-1 font-semibold text-foreground first:mt-0" />,
  h4: ({ node: _, ...p }) => <h4 {...p} className="mt-2.5 mb-1 font-medium text-foreground first:mt-0" />,
  blockquote: ({ node: _, ...p }) => <blockquote {...p} className="my-1.5 border-l-2 border-border-strong pl-3 text-muted-foreground" />,
  pre: ({ node: _, ...p }) => <pre {...p} className="my-1.5 overflow-x-auto rounded-lg border border-border bg-muted/50 px-3 py-2 font-mono text-[11px] leading-relaxed [&_code]:bg-transparent [&_code]:p-0" />,
  table: ({ node: _, ...p }) => (
    <div className="my-1.5 overflow-x-auto">
      <table {...p} className="text-[0.92em]" />
    </div>
  ),
  th: ({ node: _, ...p }) => <th {...p} className="border-b border-border-strong px-2 py-1 text-left font-medium text-foreground" />,
  td: ({ node: _, ...p }) => <td {...p} className="border-b border-border px-2 py-1 align-top" />,
  hr: () => <hr className="my-2 border-border" />,
};

/** Markdown as blocks: paragraphs, lists, code, tables. */
export function Markdown({ children, className = "" }: { children: string; className?: string }) {
  return (
    <div className={`min-w-0 break-words ${className}`}>
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={BLOCK}>
        {children}
      </ReactMarkdown>
    </div>
  );
}

/** The inline elements that a title or label keeps; any other is unwrapped to its text. */
const INLINE_ELEMENTS = ["p", "strong", "em", "del", "code", "a"];

/** Markdown within a line, for a title or a label: emphasis, code and links, no blocks. */
export function InlineMarkdown({ children }: { children: string }) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm]} allowedElements={INLINE_ELEMENTS} unwrapDisallowed components={{ ...INLINE, p: ({ children }) => <Fragment>{children}</Fragment> }}>
      {children}
    </ReactMarkdown>
  );
}
