import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /admin/pricing 是分区根而不是页面。跳转到模型定价表；父级 admin 守卫
 * 已经执行，非管理员不会到达这个重定向。与 /admin → /admin/users 的
 * index 约定一致。
 */
export const Route = createFileRoute('/_console/admin/pricing/')({
  beforeLoad: () => {
    throw redirect({ to: '/admin/pricing/model-pricing' })
  },
})
