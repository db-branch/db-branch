import { t } from "../lib/i18n";

// Whether a Branch records its changes is decided when it is created and
// never changes, so it is only ever shown, never edited.
export const recordingTitle = () => t("Recording: on, since the Branch was created. Its changes are recorded; it can be restored to any point.");

// RecordingBadge is the small marker beside a recorded Branch's name.
export function RecordingBadge({ record }: { record?: boolean }) {
  if (!record) return null;
  return (
    <span className="badge on" title={recordingTitle()}>
      recording
    </span>
  );
}

// RecordingValue is the Recording fact of a Branch's details.
export function RecordingValue({ record }: { record?: boolean }) {
  return record ? (
    <>
      on <span className="muted">{t("(since creation)")}</span>
    </>
  ) : (
    <>off</>
  );
}
