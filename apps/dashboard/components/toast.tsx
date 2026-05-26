// SPDX-License-Identifier: BUSL-1.1
"use client";

import { AnimatePresence, motion } from "motion/react";
import { CheckCircle2, Info, XCircle } from "lucide-react";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
} from "react";

type Tone = "ok" | "err" | "info";

type Toast = { id: number; tone: Tone; title: string; sub?: string };

type Ctx = {
  toast: (t: Omit<Toast, "id">) => void;
};

const ToastContext = createContext<Ctx | null>(null);

export function useToast() {
  const c = useContext(ToastContext);
  if (!c) throw new Error("useToast outside <ToastProvider>");
  return c;
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);

  const toast = useCallback((t: Omit<Toast, "id">) => {
    const id = Date.now() + Math.random();
    setItems((xs) => [...xs, { ...t, id }]);
  }, []);

  useEffect(() => {
    if (items.length === 0) return;
    const id = setTimeout(
      () => setItems((xs) => xs.slice(1)),
      4000,
    );
    return () => clearTimeout(id);
  }, [items]);

  const icon = (t: Tone) =>
    t === "ok" ? (
      <CheckCircle2 className="h-3.5 w-3.5 text-[var(--color-signal)]" />
    ) : t === "err" ? (
      <XCircle className="h-3.5 w-3.5 text-[var(--color-accent)]" />
    ) : (
      <Info className="h-3.5 w-3.5 text-[var(--color-cool)]" />
    );

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className="pointer-events-none fixed bottom-5 right-5 z-50 flex flex-col gap-2">
        <AnimatePresence>
          {items.map((t) => (
            <motion.div
              key={t.id}
              initial={{ opacity: 0, y: 10, x: 10 }}
              animate={{ opacity: 1, y: 0, x: 0 }}
              exit={{ opacity: 0, x: 20 }}
              transition={{ duration: 0.2 }}
              className="pointer-events-auto flex min-w-[280px] items-start gap-3 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] px-3 py-2.5 shadow-xl"
            >
              <span className="mt-0.5">{icon(t.tone)}</span>
              <div className="flex-1 min-w-0">
                <div className="font-mono text-[12px] text-[var(--color-fg)]">
                  {t.title}
                </div>
                {t.sub && (
                  <div className="mt-0.5 font-mono text-[10.5px] text-[var(--color-fg-dim)]">
                    {t.sub}
                  </div>
                )}
              </div>
            </motion.div>
          ))}
        </AnimatePresence>
      </div>
    </ToastContext.Provider>
  );
}
