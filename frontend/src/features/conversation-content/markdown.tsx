import Markdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Children, isValidElement, useState, type ComponentProps, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { copyConversationText } from './clipboard'

function codeText(children: ReactNode): string {
  return Children.toArray(children)
    .map((child) => {
      if (typeof child === 'string' || typeof child === 'number') return String(child)
      return isValidElement<{ children?: ReactNode }>(child) ? codeText(child.props.children) : ''
    })
    .join('')
}

function CodeBlock({ children }: ComponentProps<'pre'>) {
  const text = codeText(children)
  const [copied, setCopied] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)
  return (
    <div className="my-3 min-w-0 rounded-lg bg-muted">
      <div className="flex justify-end px-2 pt-1">
        <Button
          type="button"
          variant="ghost"
          size="xs"
          onClick={() => {
            setFailed(false)
            void copyConversationText(text).then(
              () => setCopied(text),
              () => setFailed(true),
            )
          }}
        >
          {copied === text ? '已复制' : '复制代码'}
        </Button>
      </div>
      <pre>{children}</pre>
      {failed && (
        <p role="alert" className="px-3 pb-2 text-xs text-destructive">
          复制失败，请手动选择代码复制。
        </p>
      )}
    </div>
  )
}

const components: Components = {
  pre: CodeBlock,
  a: ({ children, href }) => (
    <a
      href={href}
      className="text-primary underline underline-offset-4"
      rel="noreferrer noopener"
      target="_blank"
    >
      {children}
    </a>
  ),
  img: ({ alt }) => <span className="text-muted-foreground">[图片：{alt || '图片'}]</span>,
  table: ({ children }) => (
    <div className="max-w-full overflow-x-auto">
      <table className="w-full border-collapse text-left [&_td]:border [&_td]:p-2 [&_th]:border [&_th]:bg-muted [&_th]:p-2">
        {children}
      </table>
    </div>
  ),
}

/** Renders untrusted conversation text without raw HTML, remote images or executable URLs. */
export function ConversationMarkdown({ text }: { text: string }) {
  return (
    <div className="min-w-0 space-y-3 break-words text-sm leading-7 [overflow-wrap:anywhere] [&_blockquote]:border-l-2 [&_blockquote]:pl-4 [&_blockquote]:text-muted-foreground [&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_h1]:text-xl [&_h2]:text-lg [&_h3]:font-semibold [&_li]:my-1 [&_ol]:list-decimal [&_ol]:pl-6 [&_p]:my-2 [&_pre]:max-w-full [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-4 [&_pre_code]:p-0 [&_ul]:list-disc [&_ul]:pl-6">
      <Markdown remarkPlugins={[remarkGfm]} skipHtml components={components}>
        {text}
      </Markdown>
    </div>
  )
}
