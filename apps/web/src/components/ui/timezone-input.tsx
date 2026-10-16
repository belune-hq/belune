import { Input } from "@/components/ui/input";

// IANA zone names the browser knows. Empty where Intl.supportedValuesOf is
// missing, in which case the field is a plain text input and the server (which
// validates with time.LoadLocation) remains the authority.
function supportedTimezones(): string[] {
  try {
    return ["UTC", ...Intl.supportedValuesOf("timeZone")];
  } catch {
    return ["UTC"];
  }
}

const ZONES = supportedTimezones();
const LIST_ID = "belune-timezones";

export function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

// isKnownTimezone is the client-side half of validation; the server re-checks.
export function isKnownTimezone(tz: string): boolean {
  return ZONES.includes(tz);
}

export function TimezoneInput(props: React.ComponentProps<typeof Input>) {
  return (
    <>
      <Input {...props} list={LIST_ID} autoComplete="off" spellCheck={false} />
      <datalist id={LIST_ID}>
        {ZONES.map((z) => (
          <option key={z} value={z} />
        ))}
      </datalist>
    </>
  );
}
