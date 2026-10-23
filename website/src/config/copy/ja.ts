import type { Copy } from "./types";

export const ja: Copy = {
  brand: "NEWTG",
  skipLink: "メインコンテンツへスキップ",

  meta: {
    title: "NEWTG — すべての主要モデルをひとつの API で",
    description:
      "GPT、Claude、Gemini、DeepSeek などの主要モデルをひとつの API でルーティング。スマートルーティング、自動フェイルオーバー、統一課金に対応し、コード一行でモデルを切り替えられます。",
    ogAlt: "NEWTG ホームページ：すべての主要モデルをひとつの API で",
  },

  language: {
    label: "言語",
  },

  theme: {
    label: "テーマ",
    toggle: "テーマを切り替える",
  },

  nav: {
    links: [
      { label: "モデル", href: "#models" },
      { label: "料金", href: "/pricing" },
      { label: "ドキュメント", href: "/docs" },
      { label: "コンソール", href: "/dashboard" },
    ],
    login: "サインイン",
    cta: "無料で始める",
    openMenu: "ナビゲーションメニューを開く",
    closeMenu: "ナビゲーションメニューを閉じる",
  },

  hero: {
    titleLines: ["ひとつの API、", "すべての主要モデル"],
    subtitle:
      "GPT、Claude、Gemini、DeepSeek などの主要モデルをひとつのエンドポイントでルーティング。スマートルーティング、自動フェイルオーバー、統一課金を備え、コード一行でモデルを切り替えられます。コストは透明です。",
    primaryCta: "無料で始める",
    secondaryCta: "ドキュメントを見る",
    compatLabel: "対応",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "コピー",
    copiedLabel: "コピーしました",
    copyFailedLabel: "コピーに失敗しました",
    copySuccessAnnouncement: "コードをクリップボードにコピーしました",
    copyFailureAnnouncement: "コピーに失敗しました。コードを手動で選択してください",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "こんにちは、自己紹介してください" }
    ]
  }'`,
    requestLabel: "最新のリクエスト",
    okLabel: "200 OK",
    trace: [
      { label: "ルート", value: "openai / gpt-4o" },
      { label: "初回トークン", value: "412 ms" },
      { label: "コスト", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "プラットフォーム規模",
    items: [
      { value: "100+", label: "モデル（6 社のプロバイダー）" },
      { value: "99.9%", label: "サービス稼働率" },
      { value: "10ms", label: "スマートルーティング" },
      { value: "10,000+", label: "開発者" },
    ],
    note: "数値は本番リリース時点のものです。",
  },

  models: {
    title: "必要なモデルをすべて",
    subtitle: "価格を比較してすぐに切り替え — 料金とクォータは常に可視化",
    searchLabel: "モデルを検索",
    searchPlaceholder: "GPT-4o / Claude / DeepSeek を試す",
    vendorFilterLabel: "プロバイダーで絞り込む",
    allVendors: "すべて",
    countUnit: "モデル",
    compare: "比較",
    call: "使用",
    callHref: "/dashboard",
    emptyTitle: "該当するモデルはありません。",
    emptyAction: "検索とフィルターを解除",
    emptyCatalog: "公開モデルはまだ設定されていません。",
    loadError: "モデルデータを一時的に読み込めません。",
    retry: "再試行",
    truncatedNote: "最初の 24 件を表示しています。",
    pricingLink: "料金の全体を見る",
    footnote:
      "モデル、コンテキスト、価格はバックエンドから取得しリアルタイムに同期されます。価格は 1M トークンあたりです。",
    badgeFree: "無料",
    badgePerRequest: "リクエストごと",
    stats: {
      context: "コンテキスト",
      maxOutput: "最大出力",
      input: "入力 / 1M",
      output: "出力 / 1M",
    },
    compareBar: {
      ariaLabel: "モデル比較",
      selected: "選択中",
      unit: "モデル",
      open: "比較",
      clear: "クリア",
    },
    dialog: {
      title: "モデル比較",
      close: "比較を閉じる",
      indicator: "指標",
      rows: {
        vendor: "プロバイダー",
        context: "コンテキスト",
        maxOutput: "最大出力",
        input: "入力 / 1M",
        output: "出力 / 1M",
      },
    },
  },

  features: {
    title: "モデル接続の面倒はゲートウェイに",
    subtitle: "ルーティング、フェイルオーバー、課金を同じリクエスト経路で処理します。",
    items: [
      {
        icon: "router",
        title: "賢いモデル選択",
        description: "タスクとコスト予算に合わせて最適なモデルを自動選択",
      },
      {
        icon: "failover",
        title: "自動フェイルオーバー",
        description: "モデルが利用できない瞬間に予備へ切り替え、業務を止めません",
      },
      {
        icon: "plug",
        title: "改修なしで導入",
        description: "OpenAI 互換 API — 設定一行でモデルを切り替え",
      },
      {
        icon: "gauge",
        title: "使用量を正確に管理",
        description: "リアルタイム監視、予算アラート、トークン単位の課金",
      },
    ],
  },

  performance: {
    title: "実際の呼び出し性能",
    subtitle:
      "直近 7 日の実際の呼び出しに基づく、モデル別の初回トークン遅延と出力速度",
    windowLabel: "直近 7 日",
    columns: {
      model: "モデル",
      vendor: "プロバイダー",
      ttft: "初回トークン遅延",
      tps: "出力速度",
    },
    lowSample: "サンプル不足",
    unavailable: "性能データは一時的に利用できません",
    retry: "再試行",
    empty: "表示できるモデルがありません。",
    footnote:
      "直近 7 日の実際の呼び出しの平均値です。遅延はストリーミング呼び出しの初回トークン時間（TTFT）、出力速度は総出力トークン数と総時間からの推定値です。サンプルが 20 件未満のモデルには「サンプル不足」と表示します。データがない場合は — と表示します。",
  },

  closing: {
    title: "すべてのモデルを接続する準備はできましたか？",
    cta: "無料で始める",
    note: "クレジットカード不要。登録時にテストクレジットを進呈。",
  },

  footer: {
    tagline:
      "すべての主要モデルをひとつの API で。統一接続、スマートルーティング、自動フェイルオーバー、統一課金。",
    columns: [
      {
        title: "製品",
        links: [
          { label: "モデル", href: "#models" },
          { label: "料金", href: "/pricing" },
          { label: "コンソール", href: "/dashboard" },
        ],
      },
      {
        title: "ドキュメント",
        links: [
          { label: "クイックスタート", href: "/docs" },
          { label: "API リファレンス", href: "/docs" },
          { label: "SDK とサンプル", href: "/docs" },
          { label: "変更履歴", href: "/docs" },
        ],
      },
      {
        title: "コミュニティ",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "お問い合わせ", href: "#community" },
        ],
      },
      {
        title: "法務",
        links: [
          { label: "利用規約", href: "/legal/terms" },
          { label: "プライバシーポリシー", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "ステータス", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
};
