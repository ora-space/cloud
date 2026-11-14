import { useTranslation } from "react-i18next";
import { IconInfoCircle } from "@tabler/icons-react";

/**
 * Settings the Agent inspector can point at, keyed by the name v2-server's analysis uses. Whether
 * one shows is the server's call: the inspector only knows where each would go and why.
 */
const PLACED_SETTINGS = {
  role: "settings.workflow.unapplied.role",
  mcp: "settings.workflow.unapplied.mcp",
  interactive: "settings.workflow.unapplied.interactive",
  retry: "settings.workflow.unapplied.retry",
} as const;

/** A setting name with a field of its own in the Agent inspector. */
export type PlacedUnappliedSetting = keyof typeof PLACED_SETTINGS;

/**
 * Marks one Agent setting the server says a run saves but does not apply. Renders nothing once
 * the server stops listing it.
 */
export function WorkflowUnappliedSettingHint({
  setting,
  unapplied,
}: {
  setting: PlacedUnappliedSetting;
  unapplied: readonly string[];
}) {
  const { t } = useTranslation();
  if (!unapplied.includes(setting)) return null;
  return (
    <UnappliedNote testId={`workflow-unapplied-${setting}`}>
      {t(PLACED_SETTINGS[setting])}
    </UnappliedNote>
  );
}

/**
 * Names the unapplied settings the inspector has no field for, so a setting the server starts
 * listing is still explained before the inspector learns where it belongs.
 */
export function WorkflowOtherUnappliedSettings({
  unapplied,
}: {
  unapplied: readonly string[];
}) {
  const { t } = useTranslation();
  const others = unapplied.filter((setting) => !(setting in PLACED_SETTINGS));
  if (others.length === 0) return null;
  return (
    <UnappliedNote testId="workflow-unapplied-other">
      {t("settings.workflow.unapplied.other", { settings: others.join(", ") })}
    </UnappliedNote>
  );
}

function UnappliedNote({
  testId,
  children,
}: {
  testId: string;
  children: React.ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <p
      role="note"
      data-testid={testId}
      className="mt-1 flex items-start gap-1 text-[11px] leading-4 text-muted-foreground"
    >
      <IconInfoCircle
        aria-hidden
        className="mt-px size-3 shrink-0 text-amber-600 dark:text-amber-400"
      />
      <span>
        <span className="font-medium text-amber-700 dark:text-amber-400">
          {t("settings.workflow.unapplied.badge")}
        </span>
        <span aria-hidden> · </span>
        {children}
      </span>
    </p>
  );
}
