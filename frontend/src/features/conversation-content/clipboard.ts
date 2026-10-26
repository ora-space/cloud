/** Browser boundary for an explicit copy action; rejection is surfaced by the calling component. */
export async function copyConversationText(text: string): Promise<void> {
  await navigator.clipboard.writeText(text)
}
