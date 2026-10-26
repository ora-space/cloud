# 工作流运行定义

维护不依赖 React 的图定义、变量目录、几何与纯编辑规则；不发 HTTP 请求、不持有服务端状态。

| 文件 | 职责 |
|---|---|
| `types.ts`、`definition.ts`、`graph-codec.ts`、`graph-import.ts` | 图契约、结构检查与保留文档字段的编解码 |
| `connection-validation.ts` | 端点/端口级去重与稳定连线 id；普通无端口边保留原 id 形状 |
| `condition-cases.ts` | 兼容分支配置与规范 wire 算子 |
| `node-catalog.ts`、`node-factory.ts`、`capabilities.ts` | 可创作节点、初值和能力目录 |
| `variable-*.ts`、`global-variables.ts` | 消费者可见变量和类型推导 |
| `layout.ts`、`node-geometry.ts`、`container-layout.ts`、`iteration-*.ts` | 布局、区域成员与纯图变换 |
| 其他格式化、身份与 schema 文件 | 运行展示的纯派生规则 |

依赖基础工具和静态契约；编辑器、运行页面和插件导入消费本模块。相同节点对的不同分支连线不能互相覆盖，
空端口与缺省端口等价；condition 的历史 ELSE 归一由知道节点类型的草稿层完成。
纯函数测试覆盖边界，编解码测试核对保存重载后端口和 Output 绑定仍存在。

Loop 的外部目录只提供命名 outputs，成员目录提供 carried 与容器祖先；loopExit 不声明 output。导入保留 parentId 与初始尺寸，容器成员统一按非空 containerId 优先、否则 parentId 识别。
