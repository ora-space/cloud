# projects/components：项目页展示组件

[中文](README.md) | [English](README.en.md)

## 职责

项目列表与详情页的展示组件：项目图标，以及开通失败时的提示横幅。只渲染传入的值，不取数、不发 HTTP、不判断成败——「这个项目失败了吗」由 `features/projects/provisioning` 的 hook 决定。

## 文件

| 文件 | 说明 |
| --- | --- |
| `project-icon.tsx` | 项目图标与底色，未知图标回退为默认图标 |
| `provisioning-failure-notice.tsx` | 开通失败横幅：Cloud 的原因码、可操作文案，并说明没有可选操作 |
| `*.test.tsx` | 组件测试，直接渲染组件并断言可见文本 |

## 依赖与不变量

依赖 `lucide-react`、`lib/utils` 与 `features/projects/provisioning`（只取 `ProvisioningFailure` 类型）；由 `features/projects` 的列表与详情页使用，不被更低层模块依赖。新增展示组件放进这里，并在上表登记。

横幅同时渲染原因码与文案：两者是不同的东西，成员要照着文案处理，支持要的是确切的码。它不提供重试按钮——公开的重试路由本来就拒绝 `failed` 的 Operation，给出按钮等于承诺一件后端会拒绝的事。文案与码都来自上游，组件不替 Cloud 断言它没说过的原因。

## 测试

组件本身是纯展示，测试不需要 MSW 或 provider：直接传入一个 `ProvisioningFailure`，断言横幅出现、句子与原因码都在、且没有任何按钮。空值不属于本模块的输入——调用方只在有失败时才渲染它。
