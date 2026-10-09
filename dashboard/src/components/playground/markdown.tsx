'use client';

/**
 * Render model output as Markdown.
 *
 * The tree comes from `lib/markdown.ts`; this only maps it to elements. Text is
 * passed as children, so React escapes it and no model output can become live
 * HTML. Links open in a new tab with `noopener` because they point at whatever
 * the model decided to cite.
 */
import * as React from 'react';

import type { MdBlock, MdInline } from '@/lib/markdown';
import { parseMarkdown } from '@/lib/markdown';
import { cn } from '@/lib/utils';

function Inlines({ nodes }: { nodes: MdInline[] }) {
  return (
    <>
      {nodes.map((node, index) => {
        switch (node.type) {
          case 'text':
            return <React.Fragment key={index}>{node.value}</React.Fragment>;
          case 'code':
            return (
              <code
                key={index}
                className="rounded bg-white/[0.08] px-1 py-0.5 font-mono text-[0.85em] text-neutral-100"
              >
                {node.value}
              </code>
            );
          case 'strong':
            return (
              <strong key={index} className="font-semibold text-foreground">
                <Inlines nodes={node.children} />
              </strong>
            );
          case 'em':
            return (
              <em key={index} className="italic">
                <Inlines nodes={node.children} />
              </em>
            );
          case 'link':
            return (
              <a
                key={index}
                href={node.href}
                target="_blank"
                rel="noreferrer noopener"
                className="text-primary underline underline-offset-2 hover:text-primary/80"
              >
                <Inlines nodes={node.children} />
              </a>
            );
          default:
            return null;
        }
      })}
    </>
  );
}

function CopyCode({ text }: { text: string }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <button
      type="button"
      title={copied ? 'Copied' : 'Copy code'}
      aria-label={copied ? 'Copied' : 'Copy code'}
      onClick={() => {
        void navigator.clipboard
          .writeText(text)
          .then(() => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1500);
          })
          .catch(() => undefined);
      }}
      className="rounded-md px-2 py-1 font-mono text-[10px] uppercase tracking-wide text-muted-foreground transition-colors hover:bg-white/10 hover:text-neutral-100"
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  );
}
const HEADING_SIZE: Record<number, string> = {
  1: 'text-lg',
  2: 'text-base',
  3: 'text-[15px]',
  4: 'text-sm',
  5: 'text-sm',
  6: 'text-[13px]',
};

function Block({ block }: { block: MdBlock }) {
  switch (block.type) {
    case 'heading':
      return (
        <p
          className={cn(
            'mt-4 mb-1.5 font-semibold tracking-tight text-foreground first:mt-0',
            HEADING_SIZE[block.level] ?? 'text-sm',
          )}
        >
          <Inlines nodes={block.children} />
        </p>
      );
    case 'paragraph':
      return (
        <p className="my-2 leading-relaxed first:mt-0 last:mb-0">
          <Inlines nodes={block.children} />
        </p>
      );
    case 'code':
      return (
        <div className="my-3 overflow-hidden rounded-lg border border-white/[0.08] bg-black/40">
          <div className="flex items-center justify-between border-b border-white/[0.07] px-3 py-1">
            <p className="font-mono text-[10px] uppercase tracking-wide text-muted-foreground">
              {block.language || 'code'}
            </p>
            <CopyCode text={block.value} />
          </div>
          <pre className="overflow-x-auto p-3">
            <code className="font-mono text-xs leading-relaxed text-neutral-200">
              {block.value}
            </code>
          </pre>
        </div>
      );
    case 'list':
      return block.ordered ? (
        <ol className="my-2 list-decimal space-y-1 pl-5">
          {block.items.map((item, index) => (
            <li key={index} className="leading-relaxed">
              <Inlines nodes={item} />
            </li>
          ))}
        </ol>
      ) : (
        <ul className="my-2 list-disc space-y-1 pl-5">
          {block.items.map((item, index) => (
            <li key={index} className="leading-relaxed">
              <Inlines nodes={item} />
            </li>
          ))}
        </ul>
      );
    case 'quote':
      return (
        <blockquote className="my-2 border-l-2 border-primary/40 pl-3 text-muted-foreground">
          <Inlines nodes={block.children} />
        </blockquote>
      );
    case 'rule':
      return <hr className="my-4 border-white/[0.08]" />;
    default:
      return null;
  }
}

export function Markdown({ source, className }: { source: string; className?: string }) {
  const blocks = React.useMemo(() => parseMarkdown(source), [source]);
  return (
    <div className={cn('text-[13px] text-neutral-200', className)}>
      {blocks.map((block, index) => (
        <Block key={index} block={block} />
      ))}
    </div>
  );
}
