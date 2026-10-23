import type { Copy } from "./types";

export const vi: Copy = {
  brand: "NEWTG",
  skipLink: "Chuyển đến nội dung chính",

  meta: {
    title: "NEWTG — Một API cho mọi mô hình lớn",
    description:
      "Định tuyến GPT, Claude, Gemini, DeepSeek và các mô hình lớn khác qua một API, với định tuyến thông minh, tự động chuyển đổi dự phòng và thanh toán hợp nhất. Đổi mô hình chỉ với một dòng mã.",
    ogAlt: "Trang chủ NEWTG: một API cho mọi mô hình lớn",
  },

  language: {
    label: "Ngôn ngữ",
  },

  theme: {
    label: "Giao diện",
    toggle: "Chuyển giao diện",
  },

  nav: {
    links: [
      { label: "Mô hình", href: "#models" },
      { label: "Bảng giá", href: "/pricing" },
      { label: "Tài liệu", href: "/docs" },
      { label: "Bảng điều khiển", href: "/dashboard" },
    ],
    login: "Đăng nhập",
    cta: "Bắt đầu miễn phí",
    openMenu: "Mở menu điều hướng",
    closeMenu: "Đóng menu điều hướng",
  },

  hero: {
    titleLines: ["Một API,", "mọi mô hình lớn"],
    subtitle:
      "Định tuyến GPT, Claude, Gemini, DeepSeek và các mô hình lớn khác qua một điểm truy cập. Định tuyến thông minh, tự động chuyển đổi dự phòng và thanh toán hợp nhất — đổi mô hình chỉ với một dòng mã, chi phí minh bạch.",
    primaryCta: "Bắt đầu miễn phí",
    secondaryCta: "Xem tài liệu",
    compatLabel: "Tương thích với",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "Sao chép",
    copiedLabel: "Đã sao chép",
    copyFailedLabel: "Sao chép thất bại",
    copySuccessAnnouncement: "Đã sao chép mã vào bảng nhớ tạm",
    copyFailureAnnouncement: "Sao chép thất bại; hãy chọn mã thủ công",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "Xin chào, hãy giới thiệu về bạn" }
    ]
  }'`,
    requestLabel: "Yêu cầu gần nhất",
    okLabel: "200 OK",
    trace: [
      { label: "Tuyến", value: "openai / gpt-4o" },
      { label: "Token đầu tiên", value: "412 ms" },
      { label: "Chi phí", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "Quy mô nền tảng",
    items: [
      { value: "100+", label: "mô hình từ 6 nhà cung cấp" },
      { value: "99.9%", label: "độ khả dụng dịch vụ" },
      { value: "10ms", label: "định tuyến thông minh" },
      { value: "10,000+", label: "nhà phát triển" },
    ],
    note: "Số liệu theo phiên bản phát hành chính thức.",
  },

  models: {
    title: "Mọi mô hình bạn cần",
    subtitle: "So sánh giá và chuyển đổi tức thì — giá và hạn mức luôn hiển thị",
    searchLabel: "Tìm mô hình",
    searchPlaceholder: "Thử GPT-4o / Claude / DeepSeek",
    vendorFilterLabel: "Lọc theo nhà cung cấp",
    allVendors: "Tất cả",
    countUnit: "mô hình",
    compare: "So sánh",
    call: "Dùng",
    callHref: "/dashboard",
    emptyTitle: "Không có mô hình phù hợp.",
    emptyAction: "Xóa tìm kiếm và bộ lọc",
    emptyCatalog: "Chưa có mô hình công khai nào được cấu hình.",
    loadError: "Dữ liệu mô hình tạm thời không khả dụng.",
    retry: "Thử lại",
    truncatedNote: "Đang hiển thị 24 kết quả đầu tiên.",
    pricingLink: "Xem toàn bộ bảng giá",
    footnote:
      "Mô hình, ngữ cảnh và giá lấy từ backend và đồng bộ trực tiếp; giá hiển thị trên 1M token.",
    badgeFree: "Miễn phí",
    badgePerRequest: "Theo yêu cầu",
    stats: {
      context: "Ngữ cảnh",
      maxOutput: "Đầu ra tối đa",
      input: "Đầu vào / 1M",
      output: "Đầu ra / 1M",
    },
    compareBar: {
      ariaLabel: "So sánh mô hình",
      selected: "Đã chọn",
      unit: "mô hình",
      open: "So sánh",
      clear: "Xóa",
    },
    dialog: {
      title: "So sánh mô hình",
      close: "Đóng so sánh",
      indicator: "Chỉ số",
      rows: {
        vendor: "Nhà cung cấp",
        context: "Ngữ cảnh",
        maxOutput: "Đầu ra tối đa",
        input: "Đầu vào / 1M",
        output: "Đầu ra / 1M",
      },
    },
  },

  features: {
    title: "Để cổng kết nối lo phần hạ tầng mô hình",
    subtitle: "Định tuyến, dự phòng và thanh toán diễn ra trên cùng một luồng yêu cầu.",
    items: [
      {
        icon: "router",
        title: "Chọn mô hình thông minh",
        description: "Tự động chọn mô hình tốt nhất theo tác vụ và ngân sách",
      },
      {
        icon: "failover",
        title: "Tự động chuyển đổi dự phòng",
        description: "Chuyển sang mô hình dự phòng ngay khi mô hình không khả dụng",
      },
      {
        icon: "plug",
        title: "Tích hợp không cần sửa đổi",
        description: "API tương thích OpenAI — đổi một dòng cấu hình để chuyển mô hình",
      },
      {
        icon: "gauge",
        title: "Kiểm soát mức dùng chính xác",
        description: "Giám sát trực tiếp, cảnh báo ngân sách và tính phí theo token",
      },
    ],
  },

  performance: {
    title: "Hiệu năng gọi thực tế",
    subtitle:
      "Độ trễ token đầu tiên và tốc độ đầu ra theo mô hình, từ 7 ngày gọi thực tế gần nhất",
    windowLabel: "7 ngày gần nhất",
    columns: {
      model: "Mô hình",
      vendor: "Nhà cung cấp",
      ttft: "Độ trễ token đầu tiên",
      tps: "Tốc độ đầu ra",
    },
    lowSample: "Mẫu ít",
    unavailable: "Dữ liệu hiệu năng tạm thời không khả dụng",
    retry: "Thử lại",
    empty: "Chưa có mô hình để hiển thị.",
    footnote:
      "Giá trị trung bình từ các cuộc gọi thực tế trong 7 ngày. Độ trễ là thời gian đến token đầu tiên (TTFT) với cuộc gọi streaming; tốc độ đầu ra được ước tính từ tổng token đầu ra trên tổng thời gian. Mô hình có dưới 20 mẫu được đánh dấu mẫu ít. Dữ liệu thiếu hiển thị —.",
  },

  closing: {
    title: "Sẵn sàng kết nối mọi mô hình?",
    cta: "Bắt đầu miễn phí",
    note: "Không cần thẻ tín dụng. Tặng tín dụng dùng thử khi đăng ký.",
  },

  footer: {
    tagline:
      "Một API cho mọi mô hình lớn. Truy cập hợp nhất, định tuyến thông minh, tự động dự phòng và thanh toán hợp nhất.",
    columns: [
      {
        title: "Sản phẩm",
        links: [
          { label: "Mô hình", href: "#models" },
          { label: "Bảng giá", href: "/pricing" },
          { label: "Bảng điều khiển", href: "/dashboard" },
        ],
      },
      {
        title: "Tài liệu",
        links: [
          { label: "Bắt đầu nhanh", href: "/docs" },
          { label: "Tham chiếu API", href: "/docs" },
          { label: "SDK và ví dụ", href: "/docs" },
          { label: "Nhật ký thay đổi", href: "/docs" },
        ],
      },
      {
        title: "Cộng đồng",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "Liên hệ", href: "#community" },
        ],
      },
      {
        title: "Pháp lý",
        links: [
          { label: "Điều khoản dịch vụ", href: "/legal/terms" },
          { label: "Chính sách bảo mật", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "Trạng thái", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
};
