/**
 * How the run surfaces render an instant and an id.
 *
 * One implementation because a run's timestamps and snapshot ids appear on the history row, in the
 * detail header and in the inspector; three copies would drift the moment one of them is adjusted.
 */

/**
 * A run instant as local `YYYY-MM-DD HH:MM`, or an em dash.
 *
 * An unparseable value renders as a dash rather than `Invalid Date`: the raw string came from the
 * server, and a page that cannot read it must still be readable.
 *
 * @param iso - Timestamp the API returned.
 * @returns The local date-time with minute precision.
 */
export function formatRunInstant(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return '—'
  }
  return date.toLocaleString([], {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

/**
 * A uuid shortened to its 8-character prefix.
 *
 * Snapshot ids identify a frozen version and are never typed by hand, so the prefix is enough to
 * tell two of them apart on screen while staying compact enough to sit in a row.
 *
 * @param id - The full identifier.
 * @returns At most eight characters of it.
 */
export function shortRunId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}
