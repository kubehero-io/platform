// SPDX-License-Identifier: BUSL-1.1
"use client";

import { motion } from "motion/react";

/* Skeleton primitives — used inside <Suspense fallback={...}>.
   Animate via CSS so SSR renders a non-blank shell. */

export function SkeletonRow({ cols = 7 }: { cols?: number }) {
  return (
    <tr className="border-b border-[var(--color-line)] last:border-b-0">
      {Array.from({ length: cols }).map((_, i) => (
        <td key={i} className="py-3 pr-3">
          <motion.div
            className="h-3 w-3/4 rounded-sm bg-[var(--color-line-bright)]"
            animate={{ opacity: [0.35, 0.7, 0.35] }}
            transition={{ duration: 1.4, repeat: Infinity, ease: "easeInOut" }}
          />
        </td>
      ))}
    </tr>
  );
}

export function SkeletonTable({ rows = 6, cols = 7 }: { rows?: number; cols?: number }) {
  return (
    <tbody>
      {Array.from({ length: rows }).map((_, i) => (
        <SkeletonRow key={i} cols={cols} />
      ))}
    </tbody>
  );
}

export function SkeletonKpi() {
  return (
    <div className="flex flex-col gap-2 bg-[var(--color-bg-raised)] px-5 py-4">
      <motion.div
        className="h-2.5 w-24 rounded-sm bg-[var(--color-line-bright)]"
        animate={{ opacity: [0.35, 0.7, 0.35] }}
        transition={{ duration: 1.4, repeat: Infinity }}
      />
      <motion.div
        className="h-6 w-32 rounded-sm bg-[var(--color-line-bright)]"
        animate={{ opacity: [0.35, 0.7, 0.35] }}
        transition={{ duration: 1.4, repeat: Infinity, delay: 0.15 }}
      />
      <motion.div
        className="h-2 w-40 rounded-sm bg-[var(--color-line-bright)]"
        animate={{ opacity: [0.35, 0.7, 0.35] }}
        transition={{ duration: 1.4, repeat: Infinity, delay: 0.3 }}
      />
    </div>
  );
}
