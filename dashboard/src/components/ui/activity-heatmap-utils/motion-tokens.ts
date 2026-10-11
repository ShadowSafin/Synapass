import type { Transition } from 'motion/react';

/**
 * Shared motion values for the activity heatmap.
 *
 * Kept beside the component (not in the global theme) because these numbers
 * describe one visualisation's choreography — wave timing, tooltip springs,
 * blur amounts — rather than product-wide interaction language.
 */
export const motionTokens = {
  ease: {
    enter: [0.16, 1, 0.3, 1] as [number, number, number, number],
    standard: [0.22, 1, 0.36, 1] as [number, number, number, number],
  },
  blur: {
    soft: 4,
    subtle: 2,
  },
  spring: {
    snappy: { type: 'spring', stiffness: 550, damping: 38, mass: 0.8 } as Transition,
    morph: { type: 'spring', stiffness: 320, damping: 32 } as Transition,
  },
  duration: {
    fast: 0.16,
  },
};
