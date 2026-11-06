import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router'
import { ArrowUpRight, ExternalLink, ShieldAlert } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { simulateDtsSubmission } from '@/lib/demo/dts'
import { demoMockEnabled } from '@/lib/demo/flags'

/** Presents simulation feedback independently of the real Cloud task outcome. */
export function DtsReceipt({ taskId, title, description, severity, module, runId }: { taskId: string; title: string; description: string; severity: string; module: string; runId: string }) {
  const receipt = useQuery({ queryKey: ['dts-demo', taskId], queryFn: () => simulateDtsSubmission(taskId), enabled: demoMockEnabled(), retry: false, staleTime: Infinity })
  if (!demoMockEnabled()) return null
  if (receipt.isError) return <p role="alert">DTS 模拟提交失败，Cloud 任务已保留。<Button variant="outline" onClick={() => void receipt.refetch()}>重试模拟提交</Button></p>
  if (!receipt.data) return <p role="status">正在生成 DTS 模拟回执…</p>
  return <Dialog><DialogTrigger render={<Button size="xs" variant="outline" />}>DTS 演示 · {receipt.data.number}</DialogTrigger>
    <DialogContent className="sm:max-w-lg">
      <DialogHeader>
        <div className="flex items-center gap-3">
          <span className="flex size-10 items-center justify-center rounded-xl bg-warning-soft text-warning"><ShieldAlert className="size-5" /></span>
          <div>
            <DialogTitle className="text-base">DTS 模拟问题单</DialogTitle>
            <DialogDescription className="mt-1">用真实的 Cloud 任务信息生成，尚未向外部 DTS 写入。</DialogDescription>
          </div>
        </div>
      </DialogHeader>
      <div className="rounded-xl border bg-muted/20 px-4 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <Badge variant="outline" className="border-warning/35 bg-warning/10 font-mono text-warning">{receipt.data.number}</Badge>
          <Badge variant="outline">仅演示</Badge>
          <Badge variant="outline" className="border-destructive/30 bg-destructive/10 text-destructive">{severity || '未定级'}</Badge>
        </div>
        <dl className="mt-4 grid gap-x-4 gap-y-3 text-sm sm:grid-cols-[6.5rem_minmax(0,1fr)]">
          <dt className="text-muted-foreground">标题</dt><dd className="font-medium">{title}</dd>
          <dt className="text-muted-foreground">问题描述</dt><dd>{description}</dd>
          <dt className="text-muted-foreground">所属模块</dt><dd>{module || '未指定'}</dd>
          <dt className="text-muted-foreground">处理人</dt><dd>沿用关联 Cloud 任务的负责人</dd>
        </dl>
      </div>
      <DialogFooter>
        <Link className={buttonVariants({ variant: 'outline' })} to={`/chat?run=${runId}`}>来源巡检报告<ExternalLink /></Link>
        <Link className={buttonVariants()} to={`/collaboration/tasks/${taskId}`}>打开 Cloud 任务<ArrowUpRight /></Link>
      </DialogFooter>
    </DialogContent>
  </Dialog>
}
