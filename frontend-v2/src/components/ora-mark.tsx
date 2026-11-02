import { cn } from '@/lib/utils'

/** The Ora brand mark, the same logo desktop uses; served from `public/` so it is also the tab icon. */
export function OraMark({ className }: { className?: string }) {
  return <img src="/favicon.svg" alt="Ora" className={cn('size-8 rounded-lg', className)} />
}
