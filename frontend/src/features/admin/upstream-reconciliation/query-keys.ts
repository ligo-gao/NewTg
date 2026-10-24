/**
 * 跨 Tab 共享的查询键。
 *
 * 账号列表同时被录入 Tab（账号下拉）与本 Tab（账号表格）读取：谁写出成功
 * 都必须按同一前缀失效，否则另一个 Tab 会继续显示旧账号。快照键只有录入
 * Tab 使用，仍留在它自己的模块里。
 */
export const ACCOUNTS_QUERY_KEY = ['upstream-accounts'] as const
