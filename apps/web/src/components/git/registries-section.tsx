import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { z } from "zod";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
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
import {
  Field,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { fieldError } from "@/lib/utils/field-error";
import {
  useRegistryCredentials,
  useSaveRegistryCredential,
  useDeleteRegistryCredential,
} from "@/lib/hooks/use-registry-credentials";
import type { RegistryCredential } from "@/lib/api/registry-credentials";

type Editor =
  | { kind: "create" }
  | { kind: "edit" | "delete"; credential: RegistryCredential };

export function RegistriesSection() {
  const query = useRegistryCredentials();
  const remove = useDeleteRegistryCredential();
  const [editor, setEditor] = useState<Editor | null>(null);
  return (
    <Card>
      <CardHeader>
        <CardTitle>Registries</CardTitle>
        <CardDescription>
          Credentials are shared across this installation and managed by
          administrators. Image applications use the credential matching their
          registry host. For Docker Hub, use docker.io.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {query.isLoading ? (
          <Skeleton className="h-12 w-full" />
        ) : query.isError ? (
          <p role="alert">
            Could not load registry credentials: {query.error.message}
          </p>
        ) : query.data?.length ? (
          query.data.map((credential) => (
            <div
              key={credential.id}
              className="flex flex-wrap items-center justify-between gap-3"
            >
              <div className="flex flex-col gap-1">
                <span className="font-medium">{credential.registry_host}</span>
                <span className="text-muted-foreground text-sm">
                  {credential.username}
                </span>
                <Badge variant="secondary">
                  {credential.password_set
                    ? "Password set"
                    : "Password not set"}
                </Badge>
              </div>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  aria-label={`Edit credential for ${credential.registry_host}`}
                  onClick={() => setEditor({ kind: "edit", credential })}
                >
                  Edit
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  aria-label={`Remove credential for ${credential.registry_host}`}
                  onClick={() => setEditor({ kind: "delete", credential })}
                >
                  Remove
                </Button>
              </div>
            </div>
          ))
        ) : (
          <p className="text-muted-foreground text-sm">
            No registry credentials configured. Public images can be pulled
            without credentials.
          </p>
        )}
        <Button
          variant="outline"
          className="self-start"
          onClick={() => setEditor({ kind: "create" })}
        >
          Add registry
        </Button>
      </CardContent>
      <Dialog
        open={editor !== null}
        onOpenChange={(open) => {
          if (!open) setEditor(null);
        }}
      >
        <DialogContent>
          {editor?.kind === "delete" ? (
            <>
              <DialogHeader>
                <DialogTitle>Remove registry credential?</DialogTitle>
                <DialogDescription>
                  Future pulls from {editor.credential.registry_host} will be
                  anonymous. Running applications are unaffected.
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button variant="outline" onClick={() => setEditor(null)}>
                  Cancel
                </Button>
                <Button
                  variant="destructive"
                  disabled={remove.isPending}
                  onClick={() => {
                    remove.mutate(editor.credential.id, {
                      onSuccess: () => {
                        toast.success("Registry credential removed");
                        setEditor(null);
                      },
                      onError: (error) => toast.error(error.message),
                    });
                  }}
                >
                  Remove credential
                </Button>
              </DialogFooter>
            </>
          ) : editor ? (
            <RegistryCredentialForm
              key={editor.kind === "edit" ? editor.credential.id : "new"}
              initial={editor.kind === "edit" ? editor.credential : undefined}
              onClose={() => setEditor(null)}
            />
          ) : null}
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function RegistryCredentialForm({
  initial,
  onClose,
}: {
  initial?: RegistryCredential;
  onClose: () => void;
}) {
  const save = useSaveRegistryCredential();
  const form = useForm({
    defaultValues: {
      registry_host: initial?.registry_host ?? "",
      username: initial?.username ?? "",
      password: "",
    },
    onSubmit: async ({ value }) => {
      try {
        await save.mutateAsync({
          id: initial?.id,
          data: {
            registry_host: value.registry_host.trim(),
            username: value.username.trim(),
            ...(value.password ? { password: value.password } : {}),
          },
        });
        form.reset();
        toast.success("Registry credential saved");
        onClose();
      } catch (error) {
        toast.error(
          error instanceof Error
            ? error.message
            : "Could not save registry credential",
        );
      }
    },
  });
  const validators = {
    registry_host: z
      .string()
      .trim()
      .min(1, "Registry host is required")
      .max(300)
      .refine(
        (host) =>
          !/[\s/@?#]/.test(host) &&
          (host.includes(".") || host.includes(":") || host === "localhost"),
        "Use a registry hostname with an optional port, without a URL or image path",
      ),
    username: z.string().trim().min(1, "Username is required").max(255),
    password: z
      .string()
      .max(65536)
      .refine(
        (password) => !!initial || password.length > 0,
        "Password is required",
      ),
  };
  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {initial ? "Edit registry credential" : "Add registry credential"}
        </DialogTitle>
        <DialogDescription>
          {initial
            ? "Leave password blank to keep the stored password."
            : "The password is encrypted and cannot be read back after saving."}
        </DialogDescription>
      </DialogHeader>
      <form
        className="flex flex-col gap-4"
        onSubmit={(event) => {
          event.preventDefault();
          event.stopPropagation();
          void form.handleSubmit();
        }}
      >
        <FieldGroup>
          {(["registry_host", "username", "password"] as const).map((name) => (
            <form.Field
              key={name}
              name={name}
              validators={{
                onChange: ({ value }) => {
                  const result = validators[name].safeParse(value);
                  return result.success
                    ? undefined
                    : result.error.errors[0]?.message;
                },
              }}
              children={(field) => {
                const error = fieldError(field.state.meta.errors);
                return (
                  <Field data-invalid={!!error}>
                    <FieldLabel htmlFor={`registry-${name}`}>
                      {name === "registry_host"
                        ? "Registry host"
                        : name === "username"
                          ? "Username"
                          : "Password"}
                    </FieldLabel>
                    <Input
                      id={`registry-${name}`}
                      type={name === "password" ? "password" : "text"}
                      autoComplete={
                        name === "password" ? "new-password" : "off"
                      }
                      placeholder={
                        name === "registry_host" ? "ghcr.io" : undefined
                      }
                      value={field.state.value}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                      onBlur={field.handleBlur}
                      aria-invalid={!!error}
                    />
                    {error && <FieldError>{error}</FieldError>}
                  </Field>
                );
              }}
            />
          ))}
        </FieldGroup>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={save.isPending}>
            {save.isPending ? "Saving…" : "Save credential"}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
