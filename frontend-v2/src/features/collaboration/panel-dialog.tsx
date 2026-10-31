import { useEffect, useRef, type ReactNode } from 'react'
import { X } from 'lucide-react'
import { Button } from '@/components/ui/button'

/**
 * Modal panel shared by the collaboration page's views: create, edit, detail and pickers. The
 * native dialog supplies focus containment, Escape and return-to-trigger behavior without extra
 * state.
 */
export function PanelDialog({ icon, eyebrow, title, subtitle, onClose, children }: {
  /** The kind of panel, shown as a small glyph beside the eyebrow. */
  icon: ReactNode
  /** The kind of panel in words, e.g. 小队 or 任务. */
  eyebrow: string
  title: string
  subtitle: string
  onClose: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const dialog = ref.current!
    const trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null
    dialog.showModal()
    dialog.querySelector<HTMLElement>('input, textarea, select, button')?.focus()
    return () => {
      dialog.close()
      // React may remove the dialog before native focus restoration runs.
      queueMicrotask(() => { if (trigger?.isConnected) trigger.focus() })
    }
  }, [])
  return <dialog ref={ref} className="mc-dialog collaboration-feature" aria-labelledby="mc-dialog-title" onCancel={event => { event.preventDefault(); onClose() }}>
    <header className="mc-dialog-header">
      <div>
        <span className="mc-eyebrow">{icon}{eyebrow}</span>
        <h2 id="mc-dialog-title">{title}</h2>
        <p>{subtitle}</p>
      </div>
      <Button type="button" variant="ghost" size="icon-sm" aria-label="关闭面板" onClick={onClose}><X /></Button>
    </header>
    {children}
  </dialog>
}
