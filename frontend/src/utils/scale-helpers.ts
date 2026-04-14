/**
 * Extracts the value from a Telekom Scale component event.
 * Scale components often emit custom events where the value is in `detail.value`,
 * or standard input events where the value is on the target.
 *
 * @param ev The event emitted by the Scale component
 * @returns The string value from the event, or an empty string if not found
 */
export function extractScaleValue(ev: Event): string {
  const target = ev.target as HTMLInputElement | HTMLTextAreaElement | null;
  if (target && typeof target.value === "string") {
    return target.value;
  }
  const detail = (ev as CustomEvent<{ value?: string }>).detail;
  if (detail && typeof detail.value === "string") {
    return detail.value;
  }
  return "";
}
