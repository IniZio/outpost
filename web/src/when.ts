// The When control: Now, four presets and a custom cron rule. The browser builds the rule and
// puts it into words; the server only sees a five-field cron and a time zone.

export type Mode = "now" | "daily" | "weekdays" | "mwf" | "weekly" | "custom";

export type When = { mode: Mode; time: string; cron: string };

export const zone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

const days: Record<Exclude<Mode, "now" | "custom">, { dow: string; label: string }> = {
  daily: { dow: "*", label: "Every day" },
  weekdays: { dow: "1-5", label: "Weekdays" },
  mwf: { dow: "1,3,5", label: "Mon, Wed, Fri" },
  weekly: { dow: "1", label: "Every week" },
};

export const presets: { mode: Mode; label: string }[] = [
  { mode: "now", label: "Now" },
  ...(Object.keys(days) as (keyof typeof days)[]).map((mode) => ({ mode, label: days[mode].label })),
  { mode: "custom", label: "Custom" },
];

export const nowWhen: When = { mode: "now", time: "08:00", cron: "" };

export function cronOf(w: When): string {
  if (w.mode === "now") return "";
  if (w.mode === "custom") return w.cron.trim();
  const [h, m] = w.time.split(":").map(Number);
  return `${m || 0} ${h || 0} * * ${days[w.mode].dow}`;
}

// whenOf reads a stored cron rule back into a preset when it has that exact shape.
export function whenOf(cron: string): When {
  const m = /^(\d{1,2}) (\d{1,2}) \* \* (\*|1-5|1,3,5|1)$/.exec(cron);
  if (m) {
    const mode = (Object.keys(days) as (keyof typeof days)[]).find((k) => days[k].dow === m[3]);
    if (mode) return { mode, time: `${m[2].padStart(2, "0")}:${m[1].padStart(2, "0")}`, cron };
  }
  return { mode: "custom", time: "08:00", cron };
}

export function describe(w: When): string {
  if (w.mode === "now") return "Now";
  if (w.mode === "custom") return w.cron ? `Custom: ${w.cron}` : "Custom";
  return `${days[w.mode].label} at ${w.time}`;
}

export function valid(w: When): boolean {
  if (w.mode === "now") return true;
  if (w.mode === "custom") return w.cron.trim().split(/\s+/).length === 5;
  return /^\d{1,2}:\d{2}$/.test(w.time);
}
