import { Suspense } from 'react';
import { BenchmarkView } from '@/components/views/benchmark-view';
import { CardsSkeleton } from '@/components/ui/state';

export default function BenchmarkPage() {
  return (
    <Suspense fallback={<CardsSkeleton count={4} />}>
      <BenchmarkView />
    </Suspense>
  );
}
