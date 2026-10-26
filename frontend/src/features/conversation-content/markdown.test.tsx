import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import userEvent from '@testing-library/user-event'
import { ConversationMarkdown } from './markdown'

describe('ConversationMarkdown', () => {
  it('copies code and reports denied clipboard access without losing the content', async () => {
    const user = userEvent.setup()
    render(<ConversationMarkdown text={'```python\nprint(1)\n```'} />)
    await user.click(screen.getByRole('button', { name: '复制代码' }))
    expect(await navigator.clipboard.readText()).toBe('print(1)\n')
    expect(screen.getByRole('button', { name: '已复制' })).toBeInTheDocument()
    const write = vi
      .spyOn(navigator.clipboard, 'writeText')
      .mockRejectedValueOnce(new Error('denied'))
    await user.click(screen.getByRole('button', { name: '已复制' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('复制失败')
    expect(screen.getByText('print(1)')).toBeInTheDocument()
    write.mockRestore()
  })
  it('renders formatted responses, code and tables', () => {
    render(
      <ConversationMarkdown
        text={
          '## Result\n\n**Ready**\n\n```python\nprint(1)\n```\n\n| File | Status |\n| --- | --- |\n| hello.py | done |'
        }
      />,
    )
    expect(screen.getByRole('heading', { name: 'Result' })).toBeInTheDocument()
    expect(screen.getByText('Ready').tagName).toBe('STRONG')
    expect(screen.getByText('print(1)').tagName).toBe('CODE')
    expect(screen.getByRole('table')).toHaveTextContent('hello.py')
  })

  it('does not execute HTML, expose script URLs or fetch embedded images', () => {
    const { container } = render(
      <ConversationMarkdown
        text={
          '<script>alert(1)</script>\n\n[bad](javascript:alert%281%29)\n\n![tracker](https://example.com/pixel)\n\n[safe](https://example.com)'
        }
      />,
    )
    expect(container.querySelector('script, img')).toBeNull()
    expect(screen.getByText('bad').getAttribute('href')).not.toContain('javascript:')
    expect(screen.getByRole('link', { name: 'safe' })).toHaveAttribute('rel', 'noreferrer noopener')
  })
})
