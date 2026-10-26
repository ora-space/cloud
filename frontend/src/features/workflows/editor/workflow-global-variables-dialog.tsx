import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Plus, Trash2, Variable } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select, SelectTrigger, SelectValue } from '@/components/ui/select'
import { normalizeWorkflowGlobalVariables } from '@/features/workflows/runtime/variable-catalog'
import type {
  WorkflowGlobalVariable,
  WorkflowVariableValueType,
} from '@/features/workflows/runtime/types'
import { WorkflowVariableTypeMenu } from '@/features/workflows/editor/workflow-variable-select'
import {
  formatWorkflowVariableValue,
  isWorkflowVariableValueType,
  normalizeWorkflowVariableValue,
  parseWorkflowVariableValueText,
  workflowVariableValueExample,
} from '@/features/workflows/runtime/variable-value'

/** Runtime-owned declarations the editor may read but never rename or delete. */
const SYSTEM_GLOBALS = new Set(['sys.workflow_id', 'sys.timestamp'])

/**
 * A working row: the declaration plus a stable id, so the name can stay an
 * editable field without turning the row's React key into the array position.
 */
type DraftRow = WorkflowGlobalVariable & { rowId: number }

/** One row edit a custom-variable declaration undergoes before save. */
interface WorkflowVariablePatch {
  name?: string
  value?: unknown
}

/**
 * Edits workflow-wide variables while keeping the runtime-owned system
 * declarations read-only.
 *
 * The dialog works on its own copy so cancelling discards the edits, and it
 * only commits rows whose names carry a namespace dot — the name is what a node
 * resolves the variable by. System globals are re-normalized on save, so a list
 * that omits them can never delete them from the workflow.
 *
 * @param props.open - Whether the dialog is showing.
 * @param props.variables - The declarations currently carried by the workflow.
 * @param props.onOpenChange - Receives the next open state.
 * @param props.onSave - Receives the committed list; the editor records it.
 */
export function WorkflowGlobalVariablesDialog({
  open,
  variables,
  onOpenChange,
  onSave,
}: {
  open: boolean
  variables: WorkflowGlobalVariable[]
  onOpenChange: (open: boolean) => void
  onSave: (variables: WorkflowGlobalVariable[]) => void
}) {
  const { t } = useTranslation()
  const [rows, setRows] = useState<DraftRow[]>(() => seedCustomRows(variables))
  const systemVariables = normalizeWorkflowGlobalVariables(variables).filter(isSystemGlobal)
  const hasInvalidCustomVariables = rows.some(hasInvalidRow)

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>{t('workflows.globalVariables.title')}</DialogTitle>
          <DialogDescription>{t('workflows.globalVariables.description')}</DialogDescription>
        </DialogHeader>
        <div className="max-h-[60vh] space-y-5 overflow-y-auto pr-1">
          <GlobalVariablesSystemSection variables={systemVariables} />
          <GlobalVariableCustomSection
            rows={rows}
            onPatch={patchRow}
            onChangeType={changeRowType}
            onSetValue={setRowValue}
            onRemove={removeRow}
            onAdd={addRow}
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t('workflows.list.cancel')}
          </Button>
          <Button disabled={hasInvalidCustomVariables} onClick={commit}>
            {t('workflows.globalVariables.save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )

  function patchRow(rowId: number, patch: WorkflowVariablePatch): void {
    setRows((current) => patchRowIn(current, rowId, patch))
  }

  function changeRowType(rowId: number, valueType: WorkflowVariableValueType): void {
    setRows((current) => changeRowTypeIn(current, rowId, valueType))
  }

  function setRowValue(rowId: number, text: string): void {
    setRows((current) => applyRowValueIn(current, rowId, text))
  }

  function removeRow(rowId: number): void {
    setRows((current) => current.filter((row) => row.rowId !== rowId))
  }

  function addRow(): void {
    setRows((current) => addRowIn(current))
  }

  function commit(): void {
    onSave(
      normalizeWorkflowGlobalVariables(
        rows.filter((row) => row.name.includes('.')).map(toCommittedVariable),
      ),
    )
    onOpenChange(false)
  }
}

/** The read-only runtime-provided declarations, labeled by what each provides. */
function GlobalVariablesSystemSection({ variables }: { variables: WorkflowGlobalVariable[] }) {
  const { t } = useTranslation()
  return (
    <section className="space-y-2">
      <div>
        <h3 className="text-sm font-semibold">{t('workflows.globalVariables.systemTitle')}</h3>
        <p className="mt-1 text-xs text-muted-foreground">
          {t('workflows.globalVariables.systemDescription')}
        </p>
      </div>
      {variables.map((variable) => (
        <div
          key={variable.name}
          className="rounded-lg border border-border bg-card px-3 py-2 shadow-sm"
        >
          <div className="flex items-center gap-1.5 text-sm">
            <Variable className="size-4 text-orange-600" />
            <code className="font-semibold text-foreground">{variable.name}</code>
            <span className="text-xs font-medium uppercase text-muted-foreground">
              {variable.valueType}
            </span>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            {t(
              variable.name === 'sys.workflow_id'
                ? 'workflows.globalVariables.workflowIdDescription'
                : 'workflows.globalVariables.timestampDescription',
            )}
          </p>
        </div>
      ))}
    </section>
  )
}

/** The editable custom declarations and the add button that starts a new row. */
function GlobalVariableCustomSection({
  rows,
  onPatch,
  onChangeType,
  onSetValue,
  onRemove,
  onAdd,
}: {
  rows: DraftRow[]
  onPatch: (rowId: number, patch: WorkflowVariablePatch) => void
  onChangeType: (rowId: number, valueType: WorkflowVariableValueType) => void
  onSetValue: (rowId: number, text: string) => void
  onRemove: (rowId: number) => void
  onAdd: () => void
}) {
  const { t } = useTranslation()
  return (
    <section className="space-y-2">
      <div>
        <h3 className="text-sm font-semibold">{t('workflows.globalVariables.customTitle')}</h3>
        <p className="mt-1 text-xs text-muted-foreground">
          {t('workflows.globalVariables.customDescription')}
        </p>
      </div>
      {rows.map((row) => (
        <GlobalVariableRow
          key={row.rowId}
          row={row}
          onPatch={onPatch}
          onChangeType={onChangeType}
          onSetValue={onSetValue}
          onRemove={onRemove}
        />
      ))}
      <Button type="button" variant="outline" size="sm" className="w-full" onClick={onAdd}>
        <Plus />
        {t('workflows.globalVariables.add')}
      </Button>
    </section>
  )
}

/** One custom declaration: name, type, value, and its remove control. */
function GlobalVariableRow({
  row,
  onPatch,
  onChangeType,
  onSetValue,
  onRemove,
}: {
  row: DraftRow
  onPatch: (rowId: number, patch: WorkflowVariablePatch) => void
  onChangeType: (rowId: number, valueType: WorkflowVariableValueType) => void
  onSetValue: (rowId: number, text: string) => void
  onRemove: (rowId: number) => void
}) {
  const { t } = useTranslation()
  const index = row.rowId + 1
  const valueInvalid =
    row.value !== undefined && !normalizeWorkflowVariableValue(row.value, row.valueType).valid
  return (
    <div className="space-y-1 rounded-lg border border-border p-2">
      <div className="grid grid-cols-[minmax(0,1.5fr)_minmax(130px,0.75fr)_minmax(0,1.5fr)_32px] items-center gap-2">
        <Input
          value={row.name}
          required
          aria-invalid={!row.name.includes('.')}
          aria-label={t('workflows.globalVariables.nameLabel', { index })}
          placeholder="global.variable_name"
          onChange={(event) => onPatch(row.rowId, { name: event.target.value })}
        />
        <Select
          value={row.valueType}
          onValueChange={(valueType) => {
            if (isWorkflowVariableValueType(valueType)) {
              onChangeType(row.rowId, valueType)
            }
          }}
        >
          <SelectTrigger aria-label={t('workflows.globalVariables.typeLabel', { index })}>
            <SelectValue />
          </SelectTrigger>
          <WorkflowVariableTypeMenu />
        </Select>
        <Input
          value={formatWorkflowVariableValue(row.value, row.valueType)}
          required
          aria-invalid={
            row.value === undefined ||
            !normalizeWorkflowVariableValue(row.value, row.valueType).valid
          }
          type={row.valueType === 'secret' ? 'password' : 'text'}
          aria-label={t('workflows.globalVariables.valueLabel', { index })}
          placeholder={t('workflows.globalVariables.valueExample', {
            example: workflowVariableValueExample(row.valueType),
          })}
          onChange={(event) => onSetValue(row.rowId, event.target.value)}
        />
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label={t('workflows.globalVariables.remove', { index })}
          onClick={() => onRemove(row.rowId)}
        >
          <Trash2 />
        </Button>
      </div>
      {valueInvalid && (
        <p className="px-1 text-xs text-destructive">
          {t('workflows.globalVariables.valueInvalid', { type: row.valueType })}
        </p>
      )}
    </div>
  )
}

/** Applies one field edit to a row, reconstructing it so the list stays pure. */
function patchRowIn(
  rows: readonly DraftRow[],
  rowId: number,
  patch: WorkflowVariablePatch,
): DraftRow[] {
  return rows.map((row) => {
    if (row.rowId !== rowId) {
      return row
    }
    const next: DraftRow = { rowId: row.rowId, name: row.name, valueType: row.valueType }
    if (row.value !== undefined) {
      next.value = row.value
    }
    if (patch.name !== undefined) {
      next.name = patch.name
    }
    if (patch.value !== undefined) {
      next.value = patch.value
    }
    return next
  })
}

/** Swaps a row's type and drops the value it can no longer represent. */
function changeRowTypeIn(
  rows: readonly DraftRow[],
  rowId: number,
  valueType: WorkflowVariableValueType,
): DraftRow[] {
  return rows.map((row) => {
    if (row.rowId !== rowId) {
      return row
    }
    return { rowId: row.rowId, name: row.name, valueType }
  })
}

/** Removes a row's stored value; an absent value is a valid "not set". */
function clearRowValueIn(rows: readonly DraftRow[], rowId: number): DraftRow[] {
  return rows.map((row) => {
    if (row.rowId !== rowId) {
      return row
    }
    return { rowId: row.rowId, name: row.name, valueType: row.valueType }
  })
}

/** Parses the value field, storing normalized text or the raw author input. */
function applyRowValueIn(rows: DraftRow[], rowId: number, text: string): DraftRow[] {
  const row = rows.find((candidate) => candidate.rowId === rowId)
  if (row === undefined) {
    return rows
  }
  const result = parseWorkflowVariableValueText(text, row.valueType)
  if (!result.valid) {
    return patchRowIn(rows, rowId, { value: text })
  }
  if (result.value === undefined) {
    return clearRowValueIn(rows, rowId)
  }
  return patchRowIn(rows, rowId, { value: result.value })
}

/** Appends a fresh row whose id no existing row has claimed. */
function addRowIn(rows: readonly DraftRow[]): DraftRow[] {
  return [...rows, { rowId: nextRowId(rows), name: '', valueType: 'string' }]
}

/** Copies a working row into its durable shape, dropping the session id. */
function toCommittedVariable(row: WorkflowGlobalVariable): WorkflowGlobalVariable {
  const variable: WorkflowGlobalVariable = { name: row.name, valueType: row.valueType }
  if (row.value !== undefined) {
    variable.value = row.value
  }
  return variable
}

/** Pairs one authored declaration with its stable session id for the dialog's rows. */
function seedCustomRows(variables: readonly WorkflowGlobalVariable[]): DraftRow[] {
  return normalizeWorkflowGlobalVariables(variables)
    .filter((variable) => !isSystemGlobal(variable))
    .map(toDraftRow)
}

/** Pairs a declaration with its stable session id, `index` being its custom row number. */
function toDraftRow(variable: WorkflowGlobalVariable, index: number): DraftRow {
  const row: DraftRow = { rowId: index, name: variable.name, valueType: variable.valueType }
  if (variable.value !== undefined) {
    row.value = variable.value
  }
  return row
}

/** Whether a custom declaration blocks saving: a bare name or a wrong value. */
function hasInvalidRow(row: WorkflowGlobalVariable): boolean {
  return (
    !row.name.includes('.') ||
    row.value === undefined ||
    !normalizeWorkflowVariableValue(row.value, row.valueType).valid
  )
}

/** The smallest id never handed to a row before, so added rows stay unique. */
function nextRowId(rows: readonly DraftRow[]): number {
  return rows.reduce((max, row) => (row.rowId > max ? row.rowId : max), 0) + 1
}

/** Identifies declarations whose values and types belong to the workflow runtime. */
function isSystemGlobal(variable: WorkflowGlobalVariable): boolean {
  return SYSTEM_GLOBALS.has(variable.name)
}
