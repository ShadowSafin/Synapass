'use client';

/**
 * The Advanced settings: system prompt, sampling, response format, extra body,
 * tools and the routing constraints.
 *
 * A testing console has more controls than a composer can show without becoming
 * a wall of inputs, so the less-used half sits behind one button. Everything
 * here writes straight into `PlaygroundConfig`, so nothing is a display-only
 * control.
 *
 * The fields live in `AdvancedFields` so both surfaces share them: the anchored
 * `AdvancedPopover` next to the composer, and the `AdvancedDrawer` slide-over
 * opened from the page top bar.
 */
import { ChevronDown, X } from 'lucide-react';
import * as React from 'react';

import { Field, NumberField, SectionCard, Textarea, Toggle } from '@/components/playground/controls';
import { ToolsEditor } from '@/components/playground/tools-editor';
import { Input } from '@/components/ui/input';
import type { PlaygroundConfig } from '@/lib/playground';
import { cn } from '@/lib/utils';

/** A one-line readout of what is configured, shown when collapsed. */
export function advancedSummary(config: PlaygroundConfig): string {
  const parts: string[] = [];
  if (config.system.trim()) parts.push('system');
  if (config.temperature >= 0) parts.push(`temp ${config.temperature}`);
  if (config.topP >= 0) parts.push(`top_p ${config.topP}`);
  if (config.maxTokens > 0) parts.push(`max ${config.maxTokens}`);
  if (config.stop.trim()) parts.push('stop');
  if (config.responseFormat.trim()) parts.push('json');
  if (config.tools.trim()) parts.push('tools');
  if (config.extraBody.trim()) parts.push('extra');
  if (config.noFallback) parts.push('no fallback');
  if (config.noCache) parts.push('no cache');
  if (config.maxCostUsd >= 0) parts.push(`≤ $${config.maxCostUsd}`);
  if (config.latencyTargetMs > 0) parts.push(`≤ ${config.latencyTargetMs}ms`);
  if (config.region.trim()) parts.push(config.region.trim());
  if (config.sensitivity.trim()) parts.push(config.sensitivity.trim());
  return parts.length > 0 ? parts.join(' · ') : 'all defaults';
}

/**
 * The advanced sections without any positioning, so they can sit in a popover,
 * a drawer, or any other container.
 */
export function AdvancedFields({
  config,
  onChange,
  disabled,
}: {
  config: PlaygroundConfig;
  onChange: (patch: Partial<PlaygroundConfig>) => void;
  disabled: boolean;
}) {
  return (
    <div className="space-y-2.5">
      <SectionCard title="System prompt" summary={config.system.trim() ? 'set' : 'none'} defaultOpen>
        <Field
          label="system"
          hint="Folded in as a leading system message, ahead of the conversation."
        >
          <Textarea
            rows={3}
            spellCheck={false}
            placeholder="You are a terse assistant. Answer in one sentence."
            value={config.system}
            onChange={(event) => onChange({ system: event.target.value })}
            disabled={disabled}
          />
        </Field>
      </SectionCard>

      <SectionCard title="Sampling" summary={samplingSummary(config)}>
        <div className="grid grid-cols-1 gap-3 min-[400px]:grid-cols-2">
          <NumberField
            label="Temperature"
            value={config.temperature}
            unset={-1}
            step={0.1}
            min={0}
            onChange={(value) => onChange({ temperature: value })}
          />
          <NumberField
            label="Top P"
            value={config.topP}
            unset={-1}
            step={0.05}
            min={0}
            max={1}
            onChange={(value) => onChange({ topP: value })}
          />
        </div>
        <NumberField
          label="Max tokens"
          hint="Left empty, the policy ceiling applies."
          value={config.maxTokens}
          unset={0}
          min={0}
          onChange={(value) => onChange({ maxTokens: value })}
        />
        <Field label="Stop sequences" hint="Comma or newline separated. Sent as the stop array.">
          <Input
            placeholder="END, ###"
            value={config.stop}
            onChange={(event) => onChange({ stop: event.target.value })}
            spellCheck={false}
          />
        </Field>
      </SectionCard>

      <SectionCard
        title="Response format"
        summary={config.responseFormat.trim() ? 'structured' : 'text'}
      >
        <Field
          label="response_format"
          hint={
            <>
              A JSON object, e.g. <code className="font-mono">{'{"type":"json_object"}'}</code>.
              The gateway validates it before routing.
            </>
          }
        >
          <Textarea
            rows={3}
            spellCheck={false}
            placeholder='{"type":"json_object"}'
            value={config.responseFormat}
            onChange={(event) => onChange({ responseFormat: event.target.value })}
            disabled={disabled}
          />
        </Field>
        <Field
          label="Extra body"
          hint="A JSON object merged into the request last, for controls with no field here — top_k, min_p, repetition_penalty, seed. It cannot set model, messages or stream."
        >
          <Textarea
            rows={3}
            spellCheck={false}
            placeholder='{"top_k":40}'
            value={config.extraBody}
            onChange={(event) => onChange({ extraBody: event.target.value })}
            disabled={disabled}
          />
        </Field>
      </SectionCard>

      <SectionCard title="Tools" summary={toolSummary(config.tools)}>
        <ToolsEditor
          value={config.tools}
          onChange={(next) => onChange({ tools: next })}
          disabled={disabled}
        />
      </SectionCard>

      <SectionCard title="Routing constraints" summary={routingSummary(config)}>
        <Toggle
          label="No fallback"
          hint="Fail instead of failing over. Makes a primary-provider failure visible instead of hidden by a slow success."
          checked={config.noFallback}
          onChange={(next) => onChange({ noFallback: next })}
          disabled={disabled}
        />
        <Toggle
          label="Bypass cache"
          hint="Forces a miss, so a cached answer cannot mask live behavior."
          checked={config.noCache}
          onChange={(next) => onChange({ noCache: next })}
          disabled={disabled}
        />
        <div className="grid grid-cols-1 gap-3 min-[400px]:grid-cols-2">
          <NumberField
            label="Max cost (USD)"
            value={config.maxCostUsd}
            unset={-1}
            step={0.001}
            min={0}
            onChange={(value) => onChange({ maxCostUsd: value })}
          />
          <NumberField
            label="Latency target (ms)"
            value={config.latencyTargetMs}
            unset={0}
            step={50}
            min={0}
            onChange={(value) => onChange({ latencyTargetMs: value })}
          />
        </div>
        <div className="grid grid-cols-1 gap-3 min-[400px]:grid-cols-2">
          <Field label="Region" hint="Pins provider geography.">
            <Input
              placeholder="any"
              value={config.region}
              onChange={(event) => onChange({ region: event.target.value })}
              spellCheck={false}
            />
          </Field>
          <Field
            label="Sensitivity"
            hint="Comma separated labels; sensitive payloads skip the cache."
          >
            <Input
              placeholder="pii,public"
              value={config.sensitivity}
              onChange={(event) => onChange({ sensitivity: event.target.value })}
              spellCheck={false}
            />
          </Field>
        </div>
      </SectionCard>
    </div>
  );
}

export function AdvancedPopover({
  config,
  onChange,
  disabled,
  onClose,
  className,
}: {
  config: PlaygroundConfig;
  onChange: (patch: Partial<PlaygroundConfig>) => void;
  disabled: boolean;
  onClose: () => void;
  className?: string;
}) {
  const rootRef = React.useRef<HTMLDivElement>(null);

  // Escape closes, and a click elsewhere closes, so the panel never gets stuck
  // over the response. Both are handled here rather than by the caller so the
  // popover owns its own dismissal.
  React.useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  React.useEffect(() => {
    const onDown = (event: MouseEvent) => {
      const target = event.target as HTMLElement | null;
      // The toggle button lives outside the panel: without this exemption the
      // mousedown would close the panel and the following click would reopen
      // it, so the button would appear to do nothing.
      if (target?.closest('[data-advanced-trigger]')) return;
      if (rootRef.current && !rootRef.current.contains(target)) onClose();
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [onClose]);

  return (
    <div
      ref={rootRef}
      role="dialog"
      aria-label="Advanced request settings"
      className={cn(
        'absolute left-0 top-full z-40 mt-2 max-h-[min(70vh,640px)] w-[min(94vw,520px)] overflow-y-auto rounded-2xl border border-white/[0.1] bg-card p-4 shadow-2xl shadow-black/40',
        className,
      )}
    >
      <div className="mb-3 flex items-center justify-between gap-2 border-b border-white/[0.07] pb-2">
        <p className="text-[13px] font-semibold text-foreground">Advanced</p>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close advanced settings"
          className="flex size-7 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <X className="size-4" />
        </button>
      </div>

      <AdvancedFields config={config} onChange={onChange} disabled={disabled} />

      <p className="mt-3 flex items-start gap-1.5 text-[11px] leading-relaxed text-muted-foreground">
        <ChevronDown className="mt-px size-3 shrink-0" />
        Every field here is either a body field or a header the gateway reads; nothing on this panel
        is decorative.
      </p>
    </div>
  );
}

function samplingSummary(config: PlaygroundConfig): string {
  const parts = [
    config.temperature >= 0 ? `temp ${config.temperature}` : null,
    config.topP >= 0 ? `top_p ${config.topP}` : null,
    config.maxTokens > 0 ? `max ${config.maxTokens}` : null,
  ].filter(Boolean);
  return parts.length > 0 ? parts.join(' · ') : 'all defaults';
}

function routingSummary(config: PlaygroundConfig): string {
  const parts: string[] = [];
  if (config.noFallback) parts.push('no fallback');
  if (config.noCache) parts.push('no cache');
  if (config.maxCostUsd >= 0) parts.push(`≤ $${config.maxCostUsd}`);
  if (config.latencyTargetMs > 0) parts.push(`≤ ${config.latencyTargetMs}ms`);
  if (config.region.trim()) parts.push(config.region.trim());
  if (config.sensitivity.trim()) parts.push(config.sensitivity.trim());
  return parts.length > 0 ? parts.join(' · ') : 'unconstrained';
}

/** A one-line readout of how many tools are attached. */
function toolSummary(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) return 'none attached';
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (Array.isArray(parsed)) return `${parsed.length} attached`;
  } catch {
    // Reported in full by the editor; the summary just says it is not usable yet.
  }
  return 'unusable JSON';
}
