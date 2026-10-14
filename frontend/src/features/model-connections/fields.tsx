import { useId } from 'react'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

/** A labelled required metadata input; its value contains no credential material. */
export function ConnectionField({
  label,
  value,
  onChange,
  type = 'text',
}: {
  label: string
  value: string | number
  onChange: (value: string) => void
  type?: 'text' | 'url' | 'number'
}) {
  const id = useId()
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type={type}
        required
        value={value}
        min={type === 'number' ? 1 : undefined}
        onChange={(event) => onChange(event.target.value)}
      />
    </div>
  )
}

/** A metadata choice field whose handler receives only a declared option value. */
export function ConnectionChoice<Value extends string>({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: Value
  options: readonly { value: Value; label: string }[]
  onChange: (value: Value) => void
}) {
  const id = useId()
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Select
        value={value}
        onValueChange={(next) => {
          const option = options.find((item) => item.value === next)
          if (option) onChange(option.value)
        }}
      >
        <SelectTrigger id={id} className="w-full">
          <SelectValue>
            {options.find((option) => option.value === value)?.label ?? '请选择'}
          </SelectValue>
        </SelectTrigger>
        <SelectContent alignItemWithTrigger={false}>
          {options.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              {option.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}
