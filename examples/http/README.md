# HTTP 接入示例

从仓库根目录运行（Go 1.24+，无需模型服务或 API Key）：

```sh
go run ./examples/http -addr 127.0.0.1:8088 -db http-demo.db
```

示例使用确定性模型和一个无外部副作用的审批工具；默认监听本机。应用可以把 `newHandler` 的路由挂到自己的服务，并接入现有认证和业务授权。

| 请求 | 作用 |
|---|---|
| `POST /runs`，JSON `{"input":"demo"}` | 返回 202 和 run_id |
| `GET /runs/{id}` | 查询登记和待审批信息 |
| `GET /runs/{id}/events` | SSE，支持 Last-Event-ID 或 after 游标 |
| `POST /runs/{id}/approvals/{call}`，JSON `{"approved":true}` | 提交审批 |
| `POST /runs/{id}/cancel` | 请求取消 |
| `POST /runs/{id}/resume` | 显式重建本示例的固定配置并恢复 |

断开事件流只停止订阅，取消运行需要单独调用 cancel。恢复后应重新订阅并处理新的审批；旧审批不代表新的授权。SQLite 文件保留后，重启会标记遗留运行，再由 resume 接口继续。

测试执行：

```sh
go test ./examples/http -count=1
```

测试通过真实 HTTP 连接覆盖提交后立即查询、重复恢复冲突、SSE 断开、取消、恢复、立即审批和按游标重连。此示例固定一种运行配置；多模型应用应像 Client 手册所述自行持久化原配置并校验权限。
