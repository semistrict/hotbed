import type { ComponentProps, ReactNode } from "react";
import { cn } from "cn";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  ARCH_KEYS,
  fieldRequiresRestart,
  getArtifacts,
  getField,
  setArtifact,
  setField,
  type SettingsDraft,
} from "../lib/settings";
import {
  fieldHint,
  fieldPlaceholder,
  triInheritLabel,
  type ArtifactSpec,
  type FieldOption,
  type FieldSpec,
  type SectionSpec,
} from "../lib/settingsFields";
import { Card } from "./Panel";

/** The draft a config form edits, what it inherits, and whether the viewer may change it. */
export interface ConfigForm {
  draft: SettingsDraft;
  /** The organization defaults a node override falls back to; undefined while editing those defaults. */
  inherited?: SettingsDraft;
  disabled: boolean;
  update: (next: (d: SettingsDraft) => SettingsDraft) => void;
}

const INPUT_CLASS = "bg-background font-mono text-[12px]";

/** A labelled form control with an optional badge beside the label and a hint under it. */
export function Field({
  id,
  label,
  hint,
  badge,
  children,
  className,
}: {
  id?: string;
  label: string;
  hint?: ReactNode;
  badge?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-1.5", className)}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Label htmlFor={id} className="text-[12px] font-medium text-foreground">
          {label}
        </Label>
        {badge}
      </div>
      {children}
      {hint && <p className="font-mono text-[10.5px] leading-snug text-muted-foreground">{hint}</p>}
    </div>
  );
}

/** A plain text input styled for the settings forms. */
export function TextInput(props: ComponentProps<typeof Input>) {
  return <Input {...props} className={cn(INPUT_CLASS, props.className)} />;
}

function RestartTag() {
  return (
    <span className="rounded-full border border-warning/40 bg-warning/10 px-1.5 py-0.5 font-mono text-[9.5px] text-warning-ink">
      restart
    </span>
  );
}

const INHERIT = "__inherit__";

function OptionSelect({
  id,
  options,
  value,
  disabled,
  onChange,
}: {
  id: string;
  options: readonly FieldOption[];
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  // Base UI's Select cannot hold "", so the inherit option travels as a sentinel.
  const items = options.map((o) => ({ value: o.value || INHERIT, label: o.label }));
  return (
    <Select items={items} value={value || INHERIT} disabled={disabled} onValueChange={(v) => v && onChange(v === INHERIT ? "" : v)}>
      <SelectTrigger id={id} className={cn("w-full", INPUT_CLASS)}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent className="font-mono text-[12px]">
        {items.map((item) => (
          <SelectItem key={item.value} value={item.value} className="text-[12px]">
            {item.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function Control({ spec, value, inherited, form }: { spec: FieldSpec; value: string; inherited: string; form: ConfigForm }) {
  const set = (v: string) => form.update((d) => setField(d, spec.path, v));
  const common = { id: spec.id, value, disabled: form.disabled };
  switch (spec.kind) {
    case "tri":
      return (
        <OptionSelect
          {...common}
          options={[
            { value: "inherit", label: triInheritLabel(spec, inherited) },
            { value: "true", label: spec.tri?.trueLabel ?? "Enabled" },
            { value: "false", label: spec.tri?.falseLabel ?? "Disabled" },
          ]}
          onChange={set}
        />
      );
    case "enum":
      return <OptionSelect {...common} options={spec.options ?? []} onChange={set} />;
    case "lines":
      return (
        <Textarea
          {...common}
          rows={4}
          placeholder={fieldPlaceholder(spec, inherited)}
          onChange={(e) => set(e.target.value)}
          className={INPUT_CLASS}
        />
      );
    default: {
      const numeric =
        spec.kind === "percent"
          ? { type: "number", step: "0.5", min: "0.1", max: "100" }
          : spec.kind === "int" || spec.kind === "seconds"
            ? { type: "number", min: String(spec.min ?? 0) }
            : {};
      return (
        <TextInput {...common} {...numeric} placeholder={fieldPlaceholder(spec, inherited)} onChange={(e) => set(e.target.value)} />
      );
    }
  }
}

/** One field as its spec describes it. */
export function ConfigField({ spec, form }: { spec: FieldSpec; form: ConfigForm }) {
  const value = getField(form.draft, spec.path);
  const inherited = form.inherited ? getField(form.inherited, spec.path) : "";
  return (
    <Field
      id={spec.id}
      label={spec.label}
      hint={fieldHint(spec, value, inherited)}
      badge={fieldRequiresRestart(spec.path) ? <RestartTag /> : undefined}
      className={spec.kind === "lines" ? "col-span-full" : undefined}
    >
      <Control spec={spec} value={value} inherited={inherited} form={form} />
    </Field>
  );
}

/** A section's fields in a grid. */
export function ConfigFields({ section, form }: { section: SectionSpec; form: ConfigForm }) {
  return (
    <div className={cn("grid gap-4 sm:grid-cols-2", section.columns === 3 && "lg:grid-cols-3")}>
      {section.fields.map((spec) => (
        <ConfigField key={spec.id} spec={spec} form={form} />
      ))}
    </div>
  );
}

/** A section as a card: its title, description and fields. */
export function ConfigSection({ section, form, right }: { section: SectionSpec; form: ConfigForm; right?: ReactNode }) {
  return (
    <Card title={section.title} sub={section.sub} right={right}>
      <ConfigFields section={section} form={form} />
    </Card>
  );
}

/** A DOM-id-safe form of a title: lowercase words joined by hyphens. */
function slugify(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

const ARTIFACT_FIELDS = [
  { key: "version", label: "Version / tag", placeholder: "e.g. build-235 or cladefs-6.12" },
  { key: "sha256", label: "SHA-256 (64 hex chars)", placeholder: "64-char lowercase hex" },
  { key: "url", label: "Download URL (https://...)", placeholder: "https://github.com/.../releases/download/..." },
  { key: "path", label: "Controller-relative fallback path", placeholder: "/builds/N/..." },
] as const;

/** A card editing one managed artifact per architecture. */
export function ArtifactEditor({ spec, form }: { spec: ArtifactSpec; form: ConfigForm }) {
  const idBase = slugify(spec.title);
  const map = getArtifacts(form.draft, spec.path);
  const inheritedMap = form.inherited ? getArtifacts(form.inherited, spec.path) : undefined;
  return (
    <Card title={spec.title} sub={spec.sub}>
      <div className="space-y-4">
        {ARCH_KEYS.map((arch) => {
          const item = map[arch];
          const inh = inheritedMap?.[arch];
          const hasValue = Boolean(item.url || item.sha256 || item.version || item.path);
          const patch = (p: Partial<typeof item>) => form.update((d) => setArtifact(d, spec.path, arch, p));
          return (
            <div key={arch} className="rounded-xl border border-border/80 bg-background/40 p-3.5">
              <div className="mb-3 flex items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  <Badge variant="secondary" className="font-mono text-[10.5px]">
                    {arch}
                  </Badge>
                  {hasValue ? (
                    <span className="font-mono text-[10.5px] text-primary">configured</span>
                  ) : inh && (inh.url || inh.path) ? (
                    <span className="font-mono text-[10.5px] text-muted-foreground">
                      inherited ({inh.version || inh.url || inh.path})
                    </span>
                  ) : (
                    <span className="font-mono text-[10.5px] text-faint">unset</span>
                  )}
                </div>
                {hasValue && !form.disabled && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="xs"
                    onClick={() => patch({ url: "", sha256: "", version: "", path: "" })}
                    className="font-mono text-[10.5px] text-muted-foreground hover:text-critical"
                  >
                    Clear {arch}
                  </Button>
                )}
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                {ARTIFACT_FIELDS.map((f) => {
                  const id = `${idBase}-${arch}-${f.key}`;
                  const inherited = inh?.[f.key] ?? "";
                  return (
                    <Field key={f.key} id={id} label={f.label}>
                      <TextInput
                        id={id}
                        value={item[f.key]}
                        disabled={form.disabled}
                        placeholder={(f.key === "sha256" && inherited ? `${inherited.slice(0, 16)}…` : inherited) || f.placeholder}
                        onChange={(e) => patch({ [f.key]: e.target.value })}
                      />
                    </Field>
                  );
                })}
              </div>
            </div>
          );
        })}
      </div>
    </Card>
  );
}
