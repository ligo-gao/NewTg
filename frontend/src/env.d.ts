interface ImportMetaEnv {
  readonly PUBLIC_BASE_PATH?: string
  readonly PUBLIC_APP_VERSION?: string
  readonly PUBLIC_ENABLE_MSW?: string
  readonly PUBLIC_DEV_PROXY_TARGET?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
