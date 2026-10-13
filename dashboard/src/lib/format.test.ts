import { describe, expect, it } from 'vitest';

import {
  EMPTY,
  formatCompact,
  formatCurrency,
  formatDateTime,
  formatDecimal,
  formatDurationMs,
  formatMoney,
  formatNumber,
  formatPercent,
  formatRate,
  formatRelative,
  formatTime,
  truncateMiddle,
} from './format';

describe('formatNumber', () => {
  it('groups thousands in en-US', () => {
    expect(formatNumber(1234567)).toBe('1,234,567');
    expect(formatNumber(0)).toBe('0');
    expect(formatNumber(-1234.5)).toBe('-1,234.5');
  });

  it('renders a marker for missing input', () => {
    expect(formatNumber(null)).toBe('—');
    expect(formatNumber(undefined)).toBe('—');
    expect(formatNumber(NaN)).toBe('—');
  });
});

describe('formatCompact', () => {
  it('abbreviates large magnitudes', () => {
    expect(formatCompact(999)).toBe('999');
    expect(formatCompact(1000)).toBe('1K');
    expect(formatCompact(1500)).toBe('1.5K');
    expect(formatCompact(2000000)).toBe('2M');
  });

  it('renders a marker for missing input', () => {
    expect(formatCompact(null)).toBe('—');
    expect(formatCompact(undefined)).toBe('—');
    expect(formatCompact(NaN)).toBe('—');
  });
});

describe('formatCurrency', () => {
  it('keeps sub-cent costs legible with up to four decimals', () => {
    expect(formatCurrency(0)).toBe('$0.00');
    expect(formatCurrency(1234.5)).toBe('$1,234.50');
    // Fractions of a cent must not round to zero when there is signal.
    expect(formatCurrency(0.0005)).toBe('$0.0005');
  });

  it('rounds dust below half a ten-thousandth to zero', () => {
    expect(formatCurrency(0.00001)).toBe('$0.00');
  });

  it('renders a marker for missing input', () => {
    expect(formatCurrency(null)).toBe('—');
    expect(formatCurrency(undefined)).toBe('—');
    expect(formatCurrency(NaN)).toBe('—');
  });
});

describe('formatMoney', () => {
  it('always renders two decimals', () => {
    expect(formatMoney(0)).toBe('$0.00');
    expect(formatMoney(1234.5)).toBe('$1,234.50');
    expect(formatMoney(1234.567)).toBe('$1,234.57');
  });

  it('renders a marker for missing input', () => {
    expect(formatMoney(null)).toBe('—');
    expect(formatMoney(undefined)).toBe('—');
    expect(formatMoney(NaN)).toBe('—');
  });
});

describe('formatPercent', () => {
  it('renders a ratio in [0,1] as a percentage', () => {
    expect(formatPercent(0)).toBe('0.0%');
    expect(formatPercent(0.05)).toBe('5.0%');
    expect(formatPercent(0.12345)).toBe('12.35%');
    expect(formatPercent(1)).toBe('100.0%');
  });

  it('renders a marker for missing input', () => {
    expect(formatPercent(null)).toBe('—');
    expect(formatPercent(undefined)).toBe('—');
    expect(formatPercent(NaN)).toBe('—');
  });
});

describe('formatDecimal', () => {
  it('renders up to two decimals without trailing zeros', () => {
    expect(formatDecimal(1)).toBe('1');
    expect(formatDecimal(1.2)).toBe('1.2');
    expect(formatDecimal(1.234)).toBe('1.23');
  });

  it('renders a marker for missing input', () => {
    expect(formatDecimal(null)).toBe('—');
    expect(formatDecimal(undefined)).toBe('—');
    expect(formatDecimal(NaN)).toBe('—');
  });
});

describe('formatDurationMs', () => {
  it('picks a readable scale', () => {
    expect(formatDurationMs(0)).toBe('0µs');
    expect(formatDurationMs(0.5)).toBe('500µs');
    expect(formatDurationMs(999)).toBe('999ms');
    expect(formatDurationMs(1500)).toBe('1.50s');
    expect(formatDurationMs(15000)).toBe('15.0s');
    expect(formatDurationMs(65000)).toBe('1m 5s');
    expect(formatDurationMs(60000)).toBe('1m 0s');
  });

  it('renders a marker for missing input', () => {
    expect(formatDurationMs(null)).toBe('—');
    expect(formatDurationMs(undefined)).toBe('—');
    expect(formatDurationMs(NaN)).toBe('—');
  });
});

describe('formatRate', () => {
  it('renders per-second figures with a floor for trace traffic', () => {
    expect(formatRate(0)).toBe('0/s');
    expect(formatRate(0.005)).toBe('<0.01/s');
    expect(formatRate(2.345)).toBe('2.35/s');
  });

  it('renders a marker for missing input', () => {
    expect(formatRate(null)).toBe('—');
    expect(formatRate(undefined)).toBe('—');
    expect(formatRate(NaN)).toBe('—');
  });
});

describe('formatDateTime and formatTime', () => {
  it('renders a marker for missing or invalid input', () => {
    expect(formatDateTime(null)).toBe('—');
    expect(formatDateTime(undefined)).toBe('—');
    expect(formatDateTime('')).toBe('—');
    expect(formatDateTime('not-a-date')).toBe('—');
    expect(formatTime(null)).toBe('—');
    expect(formatTime('')).toBe('—');
    expect(formatTime('not-a-date')).toBe('—');
  });

  it('renders a valid instant instead of the marker', () => {
    const rendered = formatDateTime('2026-10-01T04:00:00Z');
    expect(rendered).not.toBe('—');
    expect(rendered).toContain('2026');
    expect(formatTime('2026-10-01T04:00:00Z')).not.toBe('—');
  });
});

describe('formatRelative', () => {
  it('renders a marker for missing or invalid input', () => {
    expect(formatRelative(null)).toBe('—');
    expect(formatRelative(undefined)).toBe('—');
    expect(formatRelative('')).toBe('—');
    expect(formatRelative('not-a-date')).toBe('—');
  });

  it('renders short relative ages', () => {
    expect(formatRelative(new Date(Date.now() - 1000).toISOString())).toBe('just now');
    expect(formatRelative(new Date(Date.now() - 30 * 1000).toISOString())).toBe('30s ago');
    expect(formatRelative(new Date(Date.now() - 3 * 60 * 1000).toISOString())).toBe('3m ago');
    expect(formatRelative(new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString())).toBe('2h ago');
    expect(formatRelative(new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString())).toBe('3d ago');
  });
});

describe('truncateMiddle', () => {
  it('leaves short identifiers alone', () => {
    expect(truncateMiddle('req_123')).toBe('req_123');
    expect(truncateMiddle('a'.repeat(28))).toBe('a'.repeat(28));
  });

  it('keeps both ends visible with an ellipsis', () => {
    const truncated = truncateMiddle('a'.repeat(20) + 'b'.repeat(20));
    expect(truncated).toContain('…');
    expect(truncated.length).toBeLessThan(40);
    expect(truncated.startsWith('a')).toBe(true);
    expect(truncated.endsWith('b')).toBe(true);
  });

  it('honours a custom width', () => {
    expect(truncateMiddle('abcdefghij', 6)).toHaveLength(6);
  });
});

describe('EMPTY', () => {
  it('is the shared empty-value marker', () => {
    expect(EMPTY).toBe('—');
  });
});
