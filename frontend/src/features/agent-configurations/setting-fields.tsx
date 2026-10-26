import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import type { AgentSetting, AgentSettingDraft, AgentSettingValue } from './draft'

/** Renders scalar declarations as labelled controls, treating author text as plain text. */
export function AgentSettingFields({
  settings,
  values,
  onChange,
}: {
  settings: AgentSetting[]
  values: AgentSettingDraft
  onChange: (id: string, value: AgentSettingValue | undefined) => void
}) {
  return settings.map((setting) => {
    const id = 'agent-setting-' + setting.id
    const value = values[setting.id]
    return (
      <div key={setting.id} className="space-y-1.5">
        <Label htmlFor={id}>
          {setting.title}
          {setting.required ? ' *' : ''}
        </Label>
        {setting.settingType === 'boolean' ? (
          <select
            id={id}
            className="h-9 w-full rounded-md border bg-background px-3 text-sm"
            value={value === undefined ? '' : String(value)}
            required={setting.required}
            onChange={(event) =>
              onChange(
                setting.id,
                event.target.value === '' ? undefined : event.target.value === 'true',
              )
            }
          >
            <option value="">{setting.required ? '请选择' : '不设置（采用默认值）'}</option>
            <option value="true">启用</option>
            <option value="false">关闭</option>
          </select>
        ) : (
          <Input
            id={id}
            type={setting.settingType === 'number' ? 'number' : 'text'}
            step={setting.settingType === 'number' ? 'any' : undefined}
            value={typeof value === 'boolean' ? '' : (value ?? '')}
            required={setting.required}
            onChange={(event) => onChange(setting.id, event.target.value)}
          />
        )}
        <p className="text-xs text-muted-foreground">{setting.description}</p>
      </div>
    )
  })
}
