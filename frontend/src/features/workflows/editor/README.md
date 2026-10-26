# 工作流编辑器

负责画布、检查器与草稿编辑；运行策略和变量目录由 runtime 提供，HTTP 由工作流边界适配器负责。

| 文件 | 职责 |
|---|---|
| `workflow-node-card.tsx`、`workflow-node-source-handles.tsx` | 卡片和按 case id 注册的 IF/ELIF/ELSE 端口 |
| `use-workflow-draft.ts` | 草稿唯一写入者、连线身份、分支删除清理、保存与历史恢复 |
| `workflow-inspector.tsx`、`workflow-*-fields.tsx` | 按节点类型组合检查器，Output 编辑命名结果绑定 |
| `workflow-variable-select.tsx` | 显示和选择消费者可见的变量，不改变持久化 selector |
| `workflow-canvas.tsx`、`iteration-*.tsx` | React Flow 装配与迭代区域交互 |
| 其余 `workflow-*` 文件 | 编辑页、历史及导入/发布/启动等编辑动作 |

依赖 runtime、UI 组件、i18n 与工作流 API 适配器；工作流页面消费本模块。分支 id 与连线端口共同持久化，
不同端口可以汇入同一节点。Output 保存原始命名绑定，未完成的行保留为草稿，发布校验负责拒绝。
测试通过真实 React Flow 端口、受控检查器及草稿保存重载验证行为，网络测试使用共享测试边界。

历史 condition 空端口仅在画布投影为 ELSE，不改写草稿。Loop 初值选择器只列外层变量，轮末选择器包含本轮成员与 carried；新建成员立即使用父容器与相对坐标。
