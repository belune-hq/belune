import { copyToClipboard } from "@/lib/utils/clipboard";
import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { CopyIcon, ShieldCheckIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Badge } from "@/components/ui/badge";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import {
  useDisableTotp,
  useEnrollTotp,
  useRegenerateRecoveryCodes,
  useTotpStatus,
  useVerifyTotpEnrollment,
} from "@/lib/hooks/use-totp";
import type { TOTPEnrollment } from "@/lib/api/totp";

/** Shown once, at the moment they are generated. There is no endpoint that can
 *  read them back — only regeneration, which replaces the set. */
function RecoveryCodes({ codes }: { codes: string[] }) {
  return (
    <div className="space-y-3">
      <div className="bg-muted grid grid-cols-2 gap-2 rounded-md p-3 font-mono text-sm">
        {codes.map((code) => (
          <span key={code}>{code}</span>
        ))}
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={async () => {
          const ok = await copyToClipboard(codes.join("\n"));
          if (ok) toast.success("Recovery codes copied");
          else toast.error("Failed to copy");
        }}
      >
        <CopyIcon aria-hidden="true" className="size-4" />
        Copy codes
      </Button>
      <p className="text-muted-foreground text-sm">
        Store these somewhere safe. Each one signs you in once if you lose your
        authenticator, and this is the only time they are shown.
      </p>
    </div>
  );
}

export function TwoFactorCard() {
  const { data: status } = useTotpStatus();
  const enabled = status?.enabled ?? false;
  // Held here, not inside the enable dialog. Turning the factor on flips this
  // card to its enabled branch, which would unmount that dialog — and the
  // codes are shown exactly once, so unmounting it loses them for good.
  const [issuedCodes, setIssuedCodes] = useState<string[] | null>(null);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheckIcon aria-hidden="true" className="size-4" />
          Two-Factor Authentication
          {enabled ? (
            <Badge variant="default">On</Badge>
          ) : (
            <Badge variant="outline">Off</Badge>
          )}
        </CardTitle>
        <CardDescription>
          A code from your authenticator app on top of your password. It also
          applies to the host shell, the most privileged action here.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {enabled ? (
          <>
            <div className="flex items-center justify-between text-sm">
              <span className="text-muted-foreground">Recovery codes left</span>
              <span className="font-mono">
                {status?.recovery_codes_remaining ?? 0} of 10
              </span>
            </div>
            <div className="flex flex-wrap gap-2">
              <RegenerateCodesDialog />
              <DisableDialog />
            </div>
          </>
        ) : (
          <EnableDialog onIssued={setIssuedCodes} />
        )}
      </CardContent>

      <Dialog
        open={issuedCodes !== null}
        onOpenChange={(next) => !next && setIssuedCodes(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Save your recovery codes</DialogTitle>
            <DialogDescription>
              Two-factor authentication is on. Signing in on your other devices
              will ask for a code.
            </DialogDescription>
          </DialogHeader>
          {issuedCodes && <RecoveryCodes codes={issuedCodes} />}
          <DialogFooter>
            <Button onClick={() => setIssuedCodes(null)}>Done</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function EnableDialog({ onIssued }: { onIssued: (codes: string[]) => void }) {
  const [open, setOpen] = useState(false);
  const [enrollment, setEnrollment] = useState<TOTPEnrollment | null>(null);
  const enroll = useEnrollTotp();
  const verify = useVerifyTotpEnrollment();

  // Two steps in one dialog: the password buys the secret, the code proves the
  // authenticator holds it.
  const passwordForm = useForm({
    defaultValues: { password: "" },
    onSubmit: async ({ value }) => {
      try {
        setEnrollment(await enroll.mutateAsync(value.password));
      } catch (err) {
        toast.error(
          err instanceof Error ? err.message : "Could not start setup",
        );
      }
    },
  });

  const codeForm = useForm({
    defaultValues: { code: "" },
    onSubmit: async ({ value }) => {
      try {
        const result = await verify.mutateAsync(value.code);
        // Hand the codes to the card before this dialog goes away with the
        // card's own re-render, then close.
        onIssued(result.recovery_codes);
        close();
        toast.success("Two-factor authentication is on");
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "Verification failed");
      }
    },
  });

  const close = () => {
    setOpen(false);
    passwordForm.reset();
    codeForm.reset();
    setEnrollment(null);
  };

  return (
    <>
      <Button onClick={() => setOpen(true)}>Set up two-factor</Button>

      <Dialog
        open={open}
        onOpenChange={(next) => (next ? setOpen(true) : close())}
      >
        <DialogContent>
          {!enrollment ? (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                e.stopPropagation();
                passwordForm.handleSubmit();
              }}
            >
              <DialogHeader>
                <DialogTitle>Confirm your password</DialogTitle>
                <DialogDescription>
                  Turning on two-factor signs out your other sessions and makes
                  this device's authenticator the one that counts, so it asks
                  for your password first.
                </DialogDescription>
              </DialogHeader>
              <passwordForm.Field
                name="password"
                validators={{
                  onChange: z.string().min(1, "Password is required"),
                }}
                children={(field) => {
                  const error = fieldError(field.state.meta.errors);
                  return (
                    <Field data-invalid={!!error} className="py-4">
                      <FieldLabel htmlFor="enroll-password">
                        Password
                      </FieldLabel>
                      <Input
                        id="enroll-password"
                        type="password"
                        autoFocus
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        aria-invalid={!!error}
                      />
                      {error && <FieldError>{error}</FieldError>}
                    </Field>
                  );
                }}
              />
              <DialogFooter>
                <Button type="button" variant="outline" onClick={close}>
                  Cancel
                </Button>
                <passwordForm.Subscribe
                  selector={(s) => s.values.password}
                  children={(password) => (
                    <Button
                      type="submit"
                      disabled={enroll.isPending || !password}
                    >
                      {enroll.isPending ? "Preparing..." : "Continue"}
                    </Button>
                  )}
                />
              </DialogFooter>
            </form>
          ) : (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                e.stopPropagation();
                codeForm.handleSubmit();
              }}
            >
              <DialogHeader>
                <DialogTitle>Scan this with your authenticator</DialogTitle>
                <DialogDescription>
                  Then enter the six-digit code it shows. Nothing changes about
                  how you sign in until that code checks out.
                </DialogDescription>
              </DialogHeader>
              <div className="space-y-4 py-4">
                {enrollment && (
                  <div className="flex flex-col items-center gap-3">
                    <img
                      src={enrollment.qr_code}
                      alt="Two-factor setup QR code"
                      className="size-48 rounded-md bg-white p-2"
                    />
                    <div className="text-center">
                      <p className="text-muted-foreground text-xs">
                        Can't scan? Enter this key instead:
                      </p>
                      <code className="font-mono text-sm break-all">
                        {enrollment.secret}
                      </code>
                    </div>
                  </div>
                )}
                <codeForm.Field
                  name="code"
                  validators={{
                    onChange: z
                      .string()
                      .regex(/^\d{6}$/, "Enter the 6-digit code"),
                  }}
                  children={(field) => {
                    const error = fieldError(field.state.meta.errors);
                    return (
                      <Field data-invalid={!!error}>
                        <FieldLabel htmlFor="totp-code">
                          Verification code
                        </FieldLabel>
                        <Input
                          id="totp-code"
                          inputMode="numeric"
                          autoComplete="one-time-code"
                          placeholder="123456"
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(e) => field.handleChange(e.target.value)}
                          aria-invalid={!!error}
                        />
                        {error && <FieldError>{error}</FieldError>}
                      </Field>
                    );
                  }}
                />
              </div>
              <DialogFooter>
                <Button type="button" variant="outline" onClick={close}>
                  Cancel
                </Button>
                <Button type="submit" disabled={verify.isPending}>
                  {verify.isPending ? "Verifying..." : "Turn on"}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}

function DisableDialog() {
  const [open, setOpen] = useState(false);
  const disable = useDisableTotp();

  const form = useForm({
    defaultValues: {
      password: "",
      code: "",
      // Losing the authenticator is the most likely reason to be turning this
      // off, so the way out cannot itself require the authenticator. The
      // endpoint takes the method as data, exactly as the login step does.
      useRecoveryCode: false,
    },
    onSubmit: async ({ value }) => {
      try {
        await disable.mutateAsync({
          password: value.password,
          code: value.code,
          method: value.useRecoveryCode ? "recovery_code" : "totp",
        });
        toast.success("Two-factor authentication is off");
        close();
      } catch (err) {
        toast.error(
          err instanceof Error ? err.message : "Could not turn it off",
        );
      }
    },
  });

  const close = () => {
    setOpen(false);
    form.reset();
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => (next ? setOpen(true) : close())}
    >
      <Button variant="outline" onClick={() => setOpen(true)}>
        Turn off
      </Button>
      <DialogContent>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            e.stopPropagation();
            form.handleSubmit();
          }}
        >
          <DialogHeader>
            <DialogTitle>Turn off two-factor authentication</DialogTitle>
            <DialogDescription>
              Your password and a current code — turning this off is the first
              thing someone using your session would try. A recovery code works
              here too, for when the authenticator is the thing you lost.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <form.Field
              name="password"
              validators={{
                onChange: z.string().min(1, "Password is required"),
              }}
              children={(field) => {
                const error = fieldError(field.state.meta.errors);
                return (
                  <Field data-invalid={!!error}>
                    <FieldLabel htmlFor="disable-password">Password</FieldLabel>
                    <Input
                      id="disable-password"
                      type="password"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                      aria-invalid={!!error}
                    />
                    {error && <FieldError>{error}</FieldError>}
                  </Field>
                );
              }}
            />
            <form.Subscribe
              selector={(s) => s.values.useRecoveryCode}
              children={(useRecoveryCode) => (
                <form.Field
                  name="code"
                  validators={{
                    onChange: z.string().min(1, "Code is required"),
                  }}
                  children={(field) => {
                    const error = fieldError(field.state.meta.errors);
                    return (
                      <Field data-invalid={!!error}>
                        <FieldLabel htmlFor="disable-code">
                          {useRecoveryCode
                            ? "Recovery code"
                            : "Verification code"}
                        </FieldLabel>
                        <Input
                          id="disable-code"
                          inputMode={useRecoveryCode ? "text" : "numeric"}
                          autoComplete="one-time-code"
                          placeholder={
                            useRecoveryCode ? "XXXX-XXXX-XXXX-XXXX" : "123456"
                          }
                          value={field.state.value}
                          onBlur={field.handleBlur}
                          onChange={(e) => field.handleChange(e.target.value)}
                          aria-invalid={!!error}
                        />
                        {error && <FieldError>{error}</FieldError>}
                        <button
                          type="button"
                          onClick={() => {
                            form.setFieldValue(
                              "useRecoveryCode",
                              !useRecoveryCode,
                            );
                            field.handleChange("");
                          }}
                          className="text-muted-foreground hover:text-foreground text-sm underline-offset-4 hover:underline"
                        >
                          {useRecoveryCode
                            ? "Use your authenticator app instead"
                            : "Lost your device? Use a recovery code"}
                        </button>
                      </Field>
                    );
                  }}
                />
              )}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={close}>
              Cancel
            </Button>
            <Button
              type="submit"
              variant="destructive"
              disabled={disable.isPending}
            >
              {disable.isPending ? "Turning off..." : "Turn off"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function RegenerateCodesDialog() {
  const [open, setOpen] = useState(false);
  const [codes, setCodes] = useState<string[] | null>(null);
  const regenerate = useRegenerateRecoveryCodes();

  const form = useForm({
    defaultValues: { password: "", code: "" },
    onSubmit: async ({ value }) => {
      try {
        const result = await regenerate.mutateAsync(value);
        setCodes(result.recovery_codes);
        form.reset();
      } catch (err) {
        toast.error(
          err instanceof Error ? err.message : "Could not regenerate",
        );
      }
    },
  });

  const close = () => {
    setOpen(false);
    form.reset();
    setCodes(null);
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => (next ? setOpen(true) : close())}
    >
      <Button variant="outline" onClick={() => setOpen(true)}>
        New recovery codes
      </Button>
      <DialogContent>
        {codes ? (
          <>
            <DialogHeader>
              <DialogTitle>Your new recovery codes</DialogTitle>
              <DialogDescription>
                The previous set no longer works.
              </DialogDescription>
            </DialogHeader>
            <RecoveryCodes codes={codes} />
            <DialogFooter>
              <Button onClick={close}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              e.stopPropagation();
              form.handleSubmit();
            }}
          >
            <DialogHeader>
              <DialogTitle>Generate new recovery codes</DialogTitle>
              <DialogDescription>
                This replaces your current codes — any you have written down
                stop working.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-4">
              <form.Field
                name="password"
                validators={{
                  onChange: z.string().min(1, "Password is required"),
                }}
                children={(field) => {
                  const error = fieldError(field.state.meta.errors);
                  return (
                    <Field data-invalid={!!error}>
                      <FieldLabel htmlFor="regen-password">Password</FieldLabel>
                      <Input
                        id="regen-password"
                        type="password"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        aria-invalid={!!error}
                      />
                      {error && <FieldError>{error}</FieldError>}
                    </Field>
                  );
                }}
              />
              <form.Field
                name="code"
                validators={{
                  onChange: z
                    .string()
                    .regex(/^\d{6}$/, "Enter the 6-digit code"),
                }}
                children={(field) => {
                  const error = fieldError(field.state.meta.errors);
                  return (
                    <Field data-invalid={!!error}>
                      <FieldLabel htmlFor="regen-code">
                        Verification code
                      </FieldLabel>
                      <Input
                        id="regen-code"
                        inputMode="numeric"
                        autoComplete="one-time-code"
                        placeholder="123456"
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(e) => field.handleChange(e.target.value)}
                        aria-invalid={!!error}
                      />
                      {error && <FieldError>{error}</FieldError>}
                    </Field>
                  );
                }}
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={close}>
                Cancel
              </Button>
              <form.Subscribe
                selector={(s) => [s.values.password, s.values.code] as const}
                children={([password, code]) => (
                  <Button
                    type="submit"
                    disabled={regenerate.isPending || !password || !code}
                  >
                    {regenerate.isPending ? "Generating..." : "Generate"}
                  </Button>
                )}
              />
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
