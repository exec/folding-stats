// The server's clock, as observed from here.
//
// Every timestamp on this page originates on the server, and browsers routinely
// disagree with it — an unsynced machine drifts minutes, and this one is not
// hypothetical: a device sixteen minutes fast renders "updated 1h 4m ago" beside a
// countdown reading twelve minutes, which implies a 76-minute publish interval and
// makes both figures look wrong when only one of them is.
//
// So relative times are measured against the server's clock rather than the local
// one. The offset is sampled from server_time on every response; the countdown does
// not need it (it works in pure elapsed-time differences, which cancel skew) but
// anything comparing an absolute timestamp to "now" does.
//
// Absolute rendering — "8/2/2026, 10:30:48 PM EDT" — is deliberately left alone. An
// instant sent by the server is the same instant whatever the local clock says, and
// formatting it is a timezone question, not a clock question.

let offsetMs = 0;
let sampled = false;

/** Record the server's clock from a response's server_time. */
export function sync(serverTimeISO) {
  const t = Date.parse(serverTimeISO);
  if (!Number.isFinite(t)) return;
  offsetMs = t - Date.now();
  sampled = true;
}

/** Now, on the server's clock. */
export function now() {
  return Date.now() + offsetMs;
}

/**
 * How far this device's clock is from the server's, in milliseconds. Positive means
 * the device is ahead. Zero until a response has been seen.
 */
export function skewMs() {
  return sampled ? -offsetMs : 0;
}
