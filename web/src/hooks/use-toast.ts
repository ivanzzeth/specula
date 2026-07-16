import * as React from 'react';

import type { ToastActionElement, ToastProps } from '@/components/ui/toast';

/**
 * A minimal toast store (the shadcn pattern, trimmed to what this app needs).
 *
 * Deviations from the stock shadcn hook, both deliberate:
 *   · TOAST_LIMIT is 3, not 1 — an operator firing several evictions must see
 *     each result, not only the last one.
 *   · REMOVE_DELAY is 5s, not ~17 minutes. The stock value is effectively
 *     "never", which silently piles up dismissed toasts in memory.
 *
 * Errors should be raised with `duration: Infinity` at the call site so a
 * failure cannot disappear before it is read.
 */
const TOAST_LIMIT = 3;
const REMOVE_DELAY = 5000;

type ToasterToast = ToastProps & {
  id: string;
  title?: React.ReactNode;
  description?: React.ReactNode;
  action?: ToastActionElement;
};

let count = 0;
function genId(): string {
  count = (count + 1) % Number.MAX_SAFE_INTEGER;
  return count.toString();
}

type State = { toasts: ToasterToast[] };

type Action =
  | { type: 'ADD'; toast: ToasterToast }
  | { type: 'UPDATE'; toast: Partial<ToasterToast> & { id: string } }
  | { type: 'DISMISS'; toastId?: string }
  | { type: 'REMOVE'; toastId?: string };

const timeouts = new Map<string, ReturnType<typeof setTimeout>>();

function scheduleRemoval(toastId: string) {
  if (timeouts.has(toastId)) return;
  timeouts.set(
    toastId,
    setTimeout(() => {
      timeouts.delete(toastId);
      dispatch({ type: 'REMOVE', toastId });
    }, REMOVE_DELAY)
  );
}

export function reducer(state: State, action: Action): State {
  switch (action.type) {
    case 'ADD':
      return { ...state, toasts: [action.toast, ...state.toasts].slice(0, TOAST_LIMIT) };

    case 'UPDATE':
      return {
        ...state,
        toasts: state.toasts.map((t) => (t.id === action.toast.id ? { ...t, ...action.toast } : t)),
      };

    case 'DISMISS': {
      const { toastId } = action;
      if (toastId) {
        scheduleRemoval(toastId);
      } else {
        state.toasts.forEach((t) => scheduleRemoval(t.id));
      }
      return {
        ...state,
        toasts: state.toasts.map((t) =>
          t.id === toastId || toastId === undefined ? { ...t, open: false } : t
        ),
      };
    }

    case 'REMOVE':
      if (action.toastId === undefined) return { ...state, toasts: [] };
      return { ...state, toasts: state.toasts.filter((t) => t.id !== action.toastId) };
  }
}

const listeners: Array<(state: State) => void> = [];
let memoryState: State = { toasts: [] };

function dispatch(action: Action) {
  memoryState = reducer(memoryState, action);
  listeners.forEach((listener) => listener(memoryState));
}

type ToastInput = Omit<ToasterToast, 'id'>;

/** toast enqueues a toast and returns handles to update or dismiss it. */
export function toast({ ...props }: ToastInput) {
  const id = genId();
  const update = (next: ToastInput) => dispatch({ type: 'UPDATE', toast: { ...next, id } });
  const dismiss = () => dispatch({ type: 'DISMISS', toastId: id });

  dispatch({
    type: 'ADD',
    toast: {
      ...props,
      id,
      open: true,
      onOpenChange: (open) => {
        if (!open) dismiss();
      },
    },
  });

  return { id, dismiss, update };
}

/** useToast subscribes a component to the toast store. */
export function useToast() {
  const [state, setState] = React.useState<State>(memoryState);

  React.useEffect(() => {
    listeners.push(setState);
    return () => {
      const index = listeners.indexOf(setState);
      if (index > -1) listeners.splice(index, 1);
    };
  }, []);

  return {
    ...state,
    toast,
    dismiss: (toastId?: string) => dispatch({ type: 'DISMISS', toastId }),
  };
}
