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
      { label: "コンソール", href: "/dashboard" },
      { label: "モデル", href: "/models" },
      { label: "料金", href: "/pricing" },
      { label: "ランキング", href: "/rankings" },
      { label: "概要", href: "/about" },
      { label: "API ドキュメント", href: "/docs" },
    ],
    login: "サインイン",
    cta: "無料で始める",
    openMenu: "ナビゲーションメニューを開く",
    closeMenu: "ナビゲーションメニューを閉じる",
    ariaLabel: "メインナビゲーション",
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
    ariaLabel: "プラットフォームの機能",
    items: [
      { value: "OpenAI", label: "主要SDK互換" },
      { value: "課金", label: "トークン単位で透明" },
      { value: "ルーティング", label: "健全性と自動切替" },
      { value: "7言語", label: "すぐに使える" },
    ],
    note: "機能とデータは実際の運用に準じます。",
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
    probe: "プローブ",
    unavailable: "性能データは一時的に利用できません",
    retry: "再試行",
    empty: "表示できるモデルがありません。",
    footnote:
      "直近 7 日の実際の呼び出しの平均値です。遅延はストリーミング呼び出しの初回トークン時間（TTFT）、出力速度は総出力トークン数と総時間からの推定値です。サンプルが 20 件未満のモデルには「サンプル不足」と表示します。データがない場合は — と表示します。",
  },

  closing: {
    title: "すべてのモデルを接続する準備はできましたか？",
    cta: "無料で始める",
    note: "クレジットカード不要。登録してすぐに開始。",
  },

  footer: {
    tagline:
      "すべての主要モデルをひとつの API で。統一接続、スマートルーティング、自動フェイルオーバー、統一課金。",
    homeAria: "NEWTG ホーム",
    columns: [
      {
        title: "製品",
        links: [
          { label: "モデル", href: "/models" },
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
  pricingPage: {
    meta: {
      title: "モデル料金 — NEWTG",
      description:
        "入力、出力、キャッシュ、リクエストごとの課金を網羅した料金表。グループ料金、通貨、単位を切り替えられ、価格はバックエンドの設定からリアルタイムに同期されます。",
      ogAlt: "NEWTG モデル料金ページ",
    },
    title: "モデル料金",
    subtitle:
      "入力、出力、キャッシュ、リクエストごとの課金を網羅した料金表です。価格はバックエンドの設定に基づき、現在のグループでリアルタイムに換算されます。任意の行を展開すると、詳細な料金とコスト見積もりを確認できます。",
    stats: {
      models: "モデル",
      vendors: "プロバイダー",
      lowest: "最低有料入力 / 1M",
      sync: "価格同期間隔",
    },
    legend: [
      "価格は 1M トークンあたり。1K 表示に切り替え可能",
      "100 クレジット = ¥1 · ドル参考値は為替レートで換算",
      "null は —、0 は「無料」と表示",
      "グループ料金 = 基準価格 × グループ倍率",
    ],
    legendSummary: "料金の説明",
    filters: {
      title: "フィルター",
      reset: "リセット",
      close: "折りたたむ",
      vendors: "プロバイダー",
      capabilities: "機能",
      context: "コンテキスト",
      endpoint: "エンドポイント",
      mode: "課金方式",
      all: "すべて",
      perToken: "トークン単位",
      perRequest: "リクエストごと",
    },
    toolbar: {
      searchPlaceholder: "モデル名、ID、プロバイダーで検索（例：GPT-4o / Claude）",
      sortName: "名前順",
      sortPriceAsc: "入力価格が安い順",
      sortPriceDesc: "入力価格が高い順",
      groupPrefix: "グループ：",
      unitM: "/ 1M",
      unitK: "/ 1K",
      dual: "$ 参考値を表示",
      table: "テーブル",
      cards: "カード",
      filters: "フィルター",
    },
    table: {
      model: "モデル",
      vendor: "プロバイダー",
      context: "コンテキスト",
      input: "入力",
      output: "出力",
      cacheRead: "キャッシュ読取",
      cacheWrite: "キャッシュ書込",
      perRequest: "リクエストごと",
      group: "グループ料金",
      actions: "比較 / 詳細",
      compare: "比較",
      expand: "料金表を展開",
      perCall: "/ 回",
      free: "無料",
      dash: "—",
      groupRatio: "×{ratio}",
    },
    cards: {
      detail: "料金表",
      context: "コンテキスト",
      maxOutput: "最大出力",
    },
    detail: {
      register: "詳細な料金",
      base: "基準価格",
      group: "グループ料金",
      input: "入力",
      output: "出力",
      cacheRead: "キャッシュ読取",
      cacheWrite: "キャッシュ書込",
      perRequest: "リクエストごと",
      rechargeNote:
        "チャージ参考：¥1 = 100 クレジット。キャッシュ読取/書込が未設定の場合は — と表示し、「無料」は有効価格がすべて 0 の場合のみ表示します。",
      estimator: "コスト見積もり",
      inputTokens: "入力トークン",
      outputTokens: "出力トークン",
      cacheHit: "キャッシュヒット率 %",
      requests: "リクエスト数",
      estimated: "予想コスト",
      inputCost: "入力（キャッシュ未使用）",
      cacheCost: "キャッシュ読取",
      outputCost: "出力",
      perRequestCost: "リクエスト単価",
      estimatorNote:
        "見積もりは現在のグループ料金を使用します。キャッシュ読取が未設定の場合は入力価格で計算します。",
      info: "モデル情報",
      mode: "課金方式",
      copyId: "モデル ID をコピー",
      copied: "コピーしました",
      viewDetail: "詳細をすべて表示",
    },
    schedule: {
      badge: "ピーク/オフピーク",
      title: "時間帯料金",
      timezone: "タイムゾーン",
      defaultMultiplier: "オフピーク時間帯",
      windowFallback: "ピーク時間帯",
      multiplier: "×{value}",
      holidayLabel: "祝日",
      holidayNone: "祝日の調整なし",
      holidayCnOffpeak: "中国の法定祝日はオフピーク料金",
      everyDay: "毎日",
      weekdays: ["月", "火", "水", "木", "金", "土", "日"],
      overnight: "翌日終了",
      footnote:
        "時刻は {timezone} 基準です。リクエストは開始時点の料金で課金されます。",
    },
    compare: {
      title: "モデル比較",
      selected: "選択中",
      unit: "モデル",
      max: "最大 {count} 件",
      open: "比較",
      clear: "クリア",
      close: "比較を閉じる",
      indicator: "指標",
      context: "コンテキスト",
      maxOutput: "最大出力",
      mode: "課金方式",
      input: "入力",
      output: "出力",
      cacheRead: "キャッシュ読取",
      cacheWrite: "キャッシュ書込",
      perRequest: "リクエストごと",
      group: "グループ料金",
    },
    states: {
      loading: "料金を読み込み中…",
      error: "料金を一時的に読み込めません。",
      retry: "再試行",
      empty: "該当するモデルはありません。",
      emptyAction: "フィルターをクリア",
      results: "{count} 件のモデル",
      perPage: "ページあたり",
      previous: "前へ",
      next: "次へ",
    },
    modeToken: "トークン単位",
    modeRequest: "リクエストごと",
    footnote:
      "価格は 60 秒ごとに同期；null は —、「無料」は有効価格がすべて 0 の場合のみ表示します。",
  },
};
