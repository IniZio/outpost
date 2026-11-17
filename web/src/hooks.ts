import { type DependencyList, useCallback, useEffect, useRef, useState } from "react";
import { ApiError, api } from "./api";

export type Poll<T> = { data?: T; error?: Error; reload: () => void };

// usePoll runs load now, again every `every` ms while `active` holds, and on reload().
export function usePoll<T>(load: () => Promise<T>, every: number, deps: DependencyList, active = true): Poll<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error>();
  const fn = useRef(load);
  fn.current = load;
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((n) => n + 1), []);
  useEffect(() => {
    let live = true;
    const run = () =>
      fn.current().then(
        (d) => live && (setData(d), setError(undefined)),
        (e) => live && setError(e),
      );
    void run();
    const t = active ? setInterval(run, every) : undefined;
    return () => {
      live = false;
      if (t) clearInterval(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [every, active, tick, ...deps]);
  return { data, error, reload };
}

export const isSignedOut = (e?: Error) => e instanceof ApiError && e.status === 401;

export function useMe() {
  return usePoll(api.me, 60000, []);
}
