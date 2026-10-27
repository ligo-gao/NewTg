import type { Copy } from "./types";

export const ru: Copy = {
  brand: "NEWTG",
  skipLink: "Перейти к основному содержимому",

  meta: {
    title: "NEWTG — Один API для всех основных моделей",
    description:
      "Маршрутизируйте GPT, Claude, Gemini, DeepSeek и другие основные модели через один API: умная маршрутизация, автоматическое переключение и единая оплата. Смена модели — одной строкой кода.",
    ogAlt: "Главная NEWTG: один API для всех основных моделей",
  },

  language: {
    label: "Язык",
  },

  theme: {
    label: "Тема",
    toggle: "Переключить тему",
  },

  nav: {
    links: [
      { label: "Консоль", href: "/dashboard" },
      { label: "Модели", href: "/models" },
      { label: "Тарифы", href: "/pricing" },
      { label: "Рейтинги", href: "/rankings" },
      { label: "О проекте", href: "/about" },
      { label: "Документация API", href: "/docs" },
    ],
    login: "Войти",
    cta: "Начать бесплатно",
    openMenu: "Открыть меню навигации",
    closeMenu: "Закрыть меню навигации",
  },

  hero: {
    titleLines: ["Один API,", "все основные модели"],
    subtitle:
      "Маршрутизируйте GPT, Claude, Gemini, DeepSeek и другие основные модели через одну точку входа. Умная маршрутизация, автоматическое переключение и единая оплата — меняйте модель одной строкой кода с прозрачной стоимостью.",
    primaryCta: "Начать бесплатно",
    secondaryCta: "Открыть документацию",
    compatLabel: "Совместимо с",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "Копировать",
    copiedLabel: "Скопировано",
    copyFailedLabel: "Не удалось скопировать",
    copySuccessAnnouncement: "Код скопирован в буфер обмена",
    copyFailureAnnouncement: "Не удалось скопировать; выделите код вручную",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "Здравствуйте, расскажите о себе" }
    ]
  }'`,
    requestLabel: "Последний запрос",
    okLabel: "200 OK",
    trace: [
      { label: "Маршрут", value: "openai / gpt-4o" },
      { label: "Первый токен", value: "412 ms" },
      { label: "Стоимость", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "Возможности платформы",
    items: [
      { value: "OpenAI", label: "Совместимые SDK" },
      { value: "Биллинг", label: "Прозрачно по токенам" },
      { value: "Роутинг", label: "Проверки и отказоустойчивость" },
      { value: "7 языков", label: "Готово к работе" },
    ],
    note: "Функции и данные соответствуют реальному развёртыванию.",
  },

  models: {
    title: "Все модели, которые вам нужны",
    subtitle: "Сравнивайте цены и переключайтесь мгновенно — тарифы и квоты всегда на виду",
    searchLabel: "Поиск моделей",
    searchPlaceholder: "Попробуйте GPT-4o / Claude / DeepSeek",
    vendorFilterLabel: "Фильтр по провайдеру",
    allVendors: "Все",
    countUnit: "моделей",
    compare: "Сравнить",
    call: "Использовать",
    callHref: "/dashboard",
    emptyTitle: "Нет подходящих моделей.",
    emptyAction: "Сбросить поиск и фильтры",
    emptyCatalog: "Публичные модели ещё не настроены.",
    loadError: "Данные моделей временно недоступны.",
    retry: "Повторить",
    truncatedNote: "Показаны первые 24 результата.",
    pricingLink: "Полные тарифы",
    footnote:
      "Модели, контекст и цены берутся из бэкенда и синхронизируются в реальном времени; цены указаны за 1M токенов.",
    badgeFree: "Бесплатно",
    badgePerRequest: "За запрос",
    stats: {
      context: "Контекст",
      maxOutput: "Макс. вывод",
      input: "Вход / 1M",
      output: "Выход / 1M",
    },
    compareBar: {
      ariaLabel: "Сравнение моделей",
      selected: "Выбрано",
      unit: "моделей",
      open: "Сравнить",
      clear: "Сбросить",
    },
    dialog: {
      title: "Сравнение моделей",
      close: "Закрыть сравнение",
      indicator: "Показатель",
      rows: {
        vendor: "Провайдер",
        context: "Контекст",
        maxOutput: "Макс. вывод",
        input: "Вход / 1M",
        output: "Выход / 1M",
      },
    },
  },

  features: {
    title: "Отдайте рутину моделей шлюзу",
    subtitle: "Маршрутизация, отказоустойчивость и оплата — в одном пути запроса.",
    items: [
      {
        icon: "router",
        title: "Умный выбор модели",
        description: "Автоматически выбирает лучшую модель под задачу и бюджет",
      },
      {
        icon: "failover",
        title: "Автоматическое переключение",
        description: "Мгновенно переходит на резервную модель при сбое",
      },
      {
        icon: "plug",
        title: "Интеграция без переделок",
        description: "API, совместимый с OpenAI — одна строка конфигурации для смены модели",
      },
      {
        icon: "gauge",
        title: "Точный контроль расхода",
        description: "Мониторинг в реальном времени, оповещения о бюджете и оплата по токенам",
      },
    ],
  },

  performance: {
    title: "Реальная производительность",
    subtitle:
      "Задержка первого токена и скорость вывода по моделям за последние 7 дней реальных вызовов",
    windowLabel: "Последние 7 дней",
    columns: {
      model: "Модель",
      vendor: "Провайдер",
      ttft: "Задержка первого токена",
      tps: "Скорость вывода",
    },
    lowSample: "Мало данных",
    probe: "Тест",
    unavailable: "Данные о производительности временно недоступны",
    retry: "Повторить",
    empty: "Пока нет моделей для отображения.",
    footnote:
      "Средние значения по реальным вызовам за 7 дней. Задержка — время до первого токена (TTFT) для потоковых вызовов; скорость вывода оценена по общему числу выходных токенов и общему времени. Модели с менее чем 20 примерами помечены. Отсутствующие данные —.",
  },

  closing: {
    title: "Готовы подключить все модели?",
    cta: "Начать бесплатно",
    note: "Без кредитной карты. Зарегистрируйтесь и начинайте.",
  },

  footer: {
    tagline:
      "Один API для всех основных моделей. Единый доступ, умная маршрутизация, автоматическое переключение и оплата.",
    columns: [
      {
        title: "Продукт",
        links: [
          { label: "Модели", href: "/models" },
          { label: "Тарифы", href: "/pricing" },
          { label: "Консоль", href: "/dashboard" },
        ],
      },
      {
        title: "Документация",
        links: [
          { label: "Быстрый старт", href: "/docs" },
          { label: "Справочник API", href: "/docs" },
          { label: "SDK и примеры", href: "/docs" },
          { label: "Журнал изменений", href: "/docs" },
        ],
      },
      {
        title: "Сообщество",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "Связаться", href: "#community" },
        ],
      },
      {
        title: "Правовая информация",
        links: [
          { label: "Условия использования", href: "/legal/terms" },
          { label: "Политика конфиденциальности", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "Статус", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
  pricingPage: {
    meta: {
      title: "Тарифы — NEWTG",
      description:
        "Полный прайс-лист по входу, выходу, кэшу и оплате за запрос: цены по группам, переключение валюты и единиц. Цены берутся из конфигурации бэкенда и синхронизируются в реальном времени.",
      ogAlt: "Страница тарифов NEWTG",
    },
    title: "Тарифы на модели",
    subtitle:
      "Полный прайс-лист по входу, выходу, кэшу и оплате за запрос. Цены берутся из конфигурации бэкенда и пересчитываются в реальном времени по текущей группе; разверните любую строку, чтобы увидеть полный ценник и оценку стоимости.",
    stats: {
      models: "моделей",
      vendors: "провайдеров",
      lowest: "Мин. платный вход / 1M",
      sync: "Интервал синхронизации цен",
    },
    legend: [
      "Цены за 1M токенов, можно переключить на вид за 1K",
      "100 кредитов = ¥1 · справочный курс USD по текущему курсу",
      "null отображается как —, 0 — как «Бесплатно»",
      "Цена группы = базовая цена × коэффициент группы",
    ],
    legendSummary: "О тарифах",
    filters: {
      title: "Фильтры",
      reset: "Сбросить",
      close: "Свернуть",
      vendors: "Провайдеры",
      capabilities: "Возможности",
      context: "Контекст",
      endpoint: "Эндпоинт",
      mode: "Способ оплаты",
      all: "Все",
      perToken: "За токен",
      perRequest: "За запрос",
    },
    toolbar: {
      searchPlaceholder: "Поиск по названию, ID или провайдеру, например GPT-4o / Claude",
      sortName: "По названию",
      sortPriceAsc: "Цена входа: по возрастанию",
      sortPriceDesc: "Цена входа: по убыванию",
      groupPrefix: "Группа: ",
      unitM: "/ 1M",
      unitK: "/ 1K",
      dual: "Показывать $ для справки",
      table: "Таблица",
      cards: "Карточки",
      filters: "Фильтры",
    },
    table: {
      model: "Модель",
      vendor: "Провайдер",
      context: "Контекст",
      input: "Вход",
      output: "Выход",
      cacheRead: "Чтение кэша",
      cacheWrite: "Запись кэша",
      perRequest: "За запрос",
      group: "Цена группы",
      actions: "Сравнить / подробнее",
      compare: "Сравнить",
      expand: "Показать полный прайс-лист",
      perCall: "/ запрос",
      free: "Бесплатно",
      dash: "—",
      groupRatio: "×{ratio}",
    },
    cards: {
      detail: "Полный прайс-лист",
      context: "Контекст",
      maxOutput: "Макс. вывод",
    },
    detail: {
      register: "Полный ценник",
      base: "Базовая цена",
      group: "Цена группы",
      input: "Вход",
      output: "Выход",
      cacheRead: "Чтение кэша",
      cacheWrite: "Запись кэша",
      perRequest: "За запрос",
      rechargeNote:
        "Пополнение: ¥1 = 100 кредитов. Чтение/запись кэша без настройки отображается как —; «Бесплатно» — только если все действующие цены равны 0.",
      estimator: "Оценка стоимости",
      inputTokens: "Входные токены",
      outputTokens: "Выходные токены",
      cacheHit: "Доля попаданий в кэш, %",
      requests: "Количество запросов",
      estimated: "Ожидаемая стоимость",
      inputCost: "Вход (без кэша)",
      cacheCost: "Чтение кэша",
      outputCost: "Выход",
      perRequestCost: "Цена за запрос",
      estimatorNote:
        "Оценка использует цену текущей группы; чтение кэша без настройки считается по цене входа.",
      info: "Информация о модели",
      mode: "Способ оплаты",
      copyId: "Копировать ID модели",
      copied: "Скопировано",
      viewDetail: "Посмотреть все детали",
    },
    schedule: {
      badge: "Пик и непик",
      title: "Ценообразование по времени",
      timezone: "Часовой пояс",
      defaultMultiplier: "Непопулярные часы",
      windowFallback: "Часы пик",
      multiplier: "×{value}",
      holidayLabel: "Праздники",
      holidayNone: "Без учёта праздников",
      holidayCnOffpeak: "Государственные праздники Китая по тарифу непик",
      everyDay: "Каждый день",
      weekdays: ["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"],
      overnight: "до следующего дня",
      footnote:
        "Время учитывается по поясу {timezone}; запрос тарифицируется по ставке на момент начала.",
    },
    compare: {
      title: "Сравнение моделей",
      selected: "Выбрано",
      unit: "моделей",
      max: "Не более {count}",
      open: "Сравнить",
      clear: "Сбросить",
      close: "Закрыть сравнение",
      indicator: "Показатель",
      context: "Контекст",
      maxOutput: "Макс. вывод",
      mode: "Способ оплаты",
      input: "Вход",
      output: "Выход",
      cacheRead: "Чтение кэша",
      cacheWrite: "Запись кэша",
      perRequest: "За запрос",
      group: "Цена группы",
    },
    states: {
      loading: "Загрузка тарифов…",
      error: "Тарифы временно недоступны.",
      retry: "Повторить",
      empty: "Нет подходящих моделей.",
      emptyAction: "Сбросить фильтры",
      results: "Всего моделей: {count}",
      perPage: "На странице",
      previous: "Назад",
      next: "Вперёд",
    },
    modeToken: "За токен",
    modeRequest: "За запрос",
    footnote:
      "Цены синхронизируются каждые 60 секунд; null отображается как —; «Бесплатно» — только если все действующие цены равны 0.",
  },
};
