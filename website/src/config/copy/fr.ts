import type { Copy } from "./types";

export const fr: Copy = {
  brand: "NEWTG",
  skipLink: "Aller au contenu principal",

  meta: {
    title: "NEWTG — Une API pour tous les grands modèles",
    description:
      "Routez GPT, Claude, Gemini, DeepSeek et les autres grands modèles via une seule API, avec routage intelligent, bascule automatique et facturation unifiée. Changez de modèle avec une ligne de code.",
    ogAlt: "Page d'accueil NEWTG : une API pour tous les grands modèles",
  },

  language: {
    label: "Langue",
  },

  theme: {
    label: "Thème",
    toggle: "Changer de thème",
  },

  nav: {
    links: [
      { label: "Console", href: "/dashboard" },
      { label: "Modèles", href: "/models" },
      { label: "Tarification", href: "/pricing" },
      { label: "Classements", href: "/rankings" },
      { label: "À propos", href: "/about" },
      { label: "Docs API", href: "/docs" },
    ],
    login: "Se connecter",
    cta: "Commencer gratuitement",
    openMenu: "Ouvrir le menu de navigation",
    closeMenu: "Fermer le menu de navigation",
    ariaLabel: "Navigation principale",
  },

  hero: {
    titleLines: ["Une API,", "tous les grands modèles"],
    subtitle:
      "Routez GPT, Claude, Gemini, DeepSeek et d'autres grands modèles via un seul point d'accès. Routage intelligent, bascule automatique et facturation unifiée — changez de modèle avec une ligne de code, à coût transparent.",
    primaryCta: "Commencer gratuitement",
    secondaryCta: "Lire la documentation",
    compatLabel: "Compatible avec",
    compat: ["OpenAI SDK", "LangChain", "Vercel AI SDK"],
    method: "POST",
    endpoint: "api.newtg.ai/v1/chat/completions",
    copyLabel: "Copier",
    copiedLabel: "Copié",
    copyFailedLabel: "Échec de la copie",
    copySuccessAnnouncement: "Code copié dans le presse-papiers",
    copyFailureAnnouncement: "Échec de la copie ; sélectionnez le code manuellement",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "messages": [
      { "role": "user", "content": "Bonjour, présentez-vous" }
    ]
  }'`,
    requestLabel: "Dernière requête",
    okLabel: "200 OK",
    trace: [
      { label: "Routage", value: "openai / gpt-4o" },
      { label: "Premier token", value: "412 ms" },
      { label: "Coût", value: "$0.0024" },
    ],
  },

  trust: {
    ariaLabel: "Capacités de la plateforme",
    items: [
      { value: "OpenAI", label: "SDK compatibles" },
      { value: "Tarifs", label: "Au token, transparente" },
      { value: "Routage", label: "Santé et bascule" },
      { value: "7 langues", label: "Prêt à l'emploi" },
    ],
    note: "Les fonctionnalités et les données suivent le déploiement réel.",
  },

  models: {
    title: "Tous les modèles dont vous avez besoin",
    subtitle: "Comparez les prix et changez instantanément — tarifs et quotas toujours visibles",
    searchLabel: "Rechercher des modèles",
    searchPlaceholder: "Essayez GPT-4o / Claude / DeepSeek",
    vendorFilterLabel: "Filtrer par fournisseur",
    allVendors: "Tous",
    countUnit: "modèles",
    compare: "Comparer",
    call: "Utiliser",
    callHref: "/dashboard",
    emptyTitle: "Aucun modèle correspondant.",
    emptyAction: "Effacer la recherche et les filtres",
    emptyCatalog: "Aucun modèle public n'est encore configuré.",
    loadError: "Les données des modèles sont temporairement indisponibles.",
    retry: "Réessayer",
    truncatedNote: "Affichage des 24 premiers résultats.",
    pricingLink: "Voir tous les tarifs",
    footnote:
      "Les modèles, le contexte et les prix proviennent du backend et se synchronisent en direct ; les prix sont affichés par million de tokens.",
    badgeFree: "Gratuit",
    badgePerRequest: "Par requête",
    stats: {
      context: "Contexte",
      maxOutput: "Sortie max",
      input: "Entrée / 1M",
      output: "Sortie / 1M",
    },
    compareBar: {
      ariaLabel: "Comparaison de modèles",
      selected: "Sélectionnés",
      unit: "modèles",
      open: "Comparer",
      clear: "Effacer",
    },
    dialog: {
      title: "Comparaison de modèles",
      close: "Fermer la comparaison",
      indicator: "Indicateur",
      rows: {
        vendor: "Fournisseur",
        context: "Contexte",
        maxOutput: "Sortie max",
        input: "Entrée / 1M",
        output: "Sortie / 1M",
      },
    },
  },

  features: {
    title: "Laissez la plomberie des modèles à la passerelle",
    subtitle: "Routage, bascule et facturation sur le même chemin de requête.",
    items: [
      {
        icon: "router",
        title: "Sélection intelligente du modèle",
        description: "Choisit automatiquement le meilleur modèle selon la tâche et le budget",
      },
      {
        icon: "failover",
        title: "Bascule automatique",
        description: "Passe à un modèle de secours dès qu'un modèle est indisponible",
      },
      {
        icon: "plug",
        title: "Intégration sans effort",
        description: "API compatible OpenAI — une ligne de configuration pour changer de modèle",
      },
      {
        icon: "gauge",
        title: "Contrôle précis de l'usage",
        description: "Suivi en direct, alertes budgétaires et facturation au token",
      },
    ],
  },

  performance: {
    title: "Performances réelles",
    subtitle:
      "Latence du premier token et vitesse de sortie par modèle, sur les 7 derniers jours d'appels réels",
    windowLabel: "7 derniers jours",
    columns: {
      model: "Modèle",
      vendor: "Fournisseur",
      ttft: "Latence du premier token",
      tps: "Vitesse de sortie",
    },
    lowSample: "Échantillon faible",
    probe: "Sonde",
    unavailable: "Données de performance temporairement indisponibles",
    retry: "Réessayer",
    empty: "Aucun modèle à afficher.",
    footnote:
      "Moyennes issues d'appels réels sur 7 jours. La latence est le temps jusqu'au premier token (TTFT) pour les appels en streaming ; la vitesse de sortie est estimée à partir des tokens de sortie totaux sur la latence totale. Les modèles avec moins de 20 échantillons sont signalés. Les données manquantes sont affichées —.",
  },

  closing: {
    title: "Prêt à connecter tous les modèles ?",
    cta: "Commencer gratuitement",
    note: "Sans carte bancaire. Inscrivez-vous et commencez.",
  },

  footer: {
    tagline:
      "Une API pour tous les grands modèles. Accès unifié, routage intelligent, bascule automatique et facturation.",
    homeAria: "Accueil NEWTG",
    columns: [
      {
        title: "Produit",
        links: [
          { label: "Modèles", href: "/models" },
          { label: "Tarification", href: "/pricing" },
          { label: "Console", href: "/dashboard" },
        ],
      },
      {
        title: "Documentation",
        links: [
          { label: "Démarrage rapide", href: "/docs" },
          { label: "Référence API", href: "/docs" },
          { label: "SDK et exemples", href: "/docs" },
          { label: "Journal des modifications", href: "/docs" },
        ],
      },
      {
        title: "Communauté",
        links: [
          { label: "GitHub", href: "#community" },
          { label: "Discord", href: "#community" },
          { label: "X", href: "#community" },
          { label: "Contact", href: "#community" },
        ],
      },
      {
        title: "Mentions légales",
        links: [
          { label: "Conditions d'utilisation", href: "/legal/terms" },
          { label: "Politique de confidentialité", href: "/legal/privacy" },
        ],
      },
    ],
    status: { label: "Statut", href: "#status" },
    copyright: "© 2026 NEWTG",
  },
  pricingPage: {
    meta: {
      title: "Tarification — NEWTG",
      description:
        "Grille tarifaire complète pour l'entrée, la sortie, le cache et les appels par requête, avec prix par groupe et unités et devises commutables. Les prix proviennent de la configuration du backend et se synchronisent en direct.",
      ogAlt: "Page de tarification NEWTG",
    },
    title: "Tarification des modèles",
    subtitle:
      "Grille tarifaire complète pour l'entrée, la sortie, le cache et les appels par requête. Les prix proviennent de la configuration du backend et sont convertis en direct selon le groupe actif ; développez n'importe quelle ligne pour voir la grille détaillée et une estimation des coûts.",
    stats: {
      models: "modèles",
      vendors: "fournisseurs",
      lowest: "Entrée payante la plus basse / 1M",
      sync: "Fréquence de synchronisation des prix",
    },
    legend: [
      "Prix par 1M tokens, avec affichage 1K disponible",
      "100 crédits = ¥1 · référence USD convertie au taux de change",
      "null affiche —, 0 affiche « Gratuit »",
      "Prix de groupe = prix de base × coefficient de groupe",
    ],
    legendSummary: "Notes de tarification",
    filters: {
      title: "Filtres",
      reset: "Réinitialiser",
      close: "Réduire",
      vendors: "Fournisseurs",
      capabilities: "Capacités",
      context: "Contexte",
      endpoint: "Point d'accès",
      mode: "Mode de facturation",
      all: "Tous",
      perToken: "Au token",
      perRequest: "Par requête",
    },
    toolbar: {
      searchPlaceholder: "Rechercher par nom, ID ou fournisseur, par ex. GPT-4o / Claude",
      sortName: "Par nom",
      sortPriceAsc: "Prix d'entrée croissant",
      sortPriceDesc: "Prix d'entrée décroissant",
      groupPrefix: "Groupe : ",
      unitM: "/ 1M",
      unitK: "/ 1K",
      dual: "Afficher la référence $",
      table: "Tableau",
      cards: "Cartes",
      filters: "Filtres",
    },
    table: {
      model: "Modèle",
      vendor: "Fournisseur",
      context: "Contexte",
      input: "Entrée",
      output: "Sortie",
      cacheRead: "Lecture cache",
      cacheWrite: "Écriture cache",
      perRequest: "Par requête",
      group: "Prix de groupe",
      actions: "Comparer / détails",
      compare: "Comparer",
      expand: "Afficher la grille complète",
      perCall: "/ appel",
      free: "Gratuit",
      dash: "—",
      groupRatio: "×{ratio}",
    },
    cards: {
      detail: "Grille complète",
      context: "Contexte",
      maxOutput: "Sortie max",
    },
    detail: {
      register: "Grille tarifaire détaillée",
      base: "Prix de base",
      group: "Prix de groupe",
      input: "Entrée",
      output: "Sortie",
      cacheRead: "Lecture cache",
      cacheWrite: "Écriture cache",
      perRequest: "Par requête",
      rechargeNote:
        "Référence de recharge : ¥1 = 100 crédits. La lecture/écriture du cache affiche — lorsqu'elle n'est pas configurée ; « Gratuit » uniquement lorsque tous les prix effectifs sont à 0.",
      estimator: "Estimation des coûts",
      inputTokens: "tokens d'entrée",
      outputTokens: "tokens de sortie",
      cacheHit: "Taux de cache %",
      requests: "Nombre de requêtes",
      estimated: "Coût estimé",
      inputCost: "Entrée (sans cache)",
      cacheCost: "Lecture cache",
      outputCost: "Sortie",
      perRequestCost: "Prix par requête",
      estimatorNote:
        "L'estimation utilise le prix du groupe actif ; une lecture cache non configurée est facturée au prix de l'entrée.",
      info: "Informations du modèle",
      mode: "Mode de facturation",
      copyId: "Copier l'ID du modèle",
      copied: "Copié",
      viewDetail: "Voir tous les détails",
    },
    schedule: {
      badge: "Heures pleines / creuses",
      title: "Tarification par plage horaire",
      timezone: "Fuseau horaire",
      defaultMultiplier: "Heures creuses",
      windowFallback: "Plage de pointe",
      multiplier: "×{value}",
      holidayLabel: "Jours fériés",
      holidayNone: "Aucun ajustement pour les jours fériés",
      holidayCnOffpeak: "Jours fériés chinois facturés au tarif creux",
      everyDay: "Tous les jours",
      weekdays: ["Lun", "Mar", "Mer", "Jeu", "Ven", "Sam", "Dim"],
      overnight: "fin le lendemain",
      footnote:
        "Les horaires suivent {timezone} ; une requête est facturée au tarif en vigueur à son démarrage.",
    },
    compare: {
      title: "Comparaison de modèles",
      selected: "Sélectionnés",
      unit: "modèles",
      max: "{count} maximum",
      open: "Comparer",
      clear: "Effacer",
      close: "Fermer la comparaison",
      indicator: "Indicateur",
      context: "Contexte",
      maxOutput: "Sortie max",
      mode: "Mode de facturation",
      input: "Entrée",
      output: "Sortie",
      cacheRead: "Lecture cache",
      cacheWrite: "Écriture cache",
      perRequest: "Par requête",
      group: "Prix de groupe",
    },
    states: {
      loading: "Chargement des tarifs…",
      error: "Les tarifs sont temporairement indisponibles.",
      retry: "Réessayer",
      empty: "Aucun modèle correspondant.",
      emptyAction: "Effacer les filtres",
      results: "{count} modèles",
      perPage: "Par page",
      previous: "Précédent",
      next: "Suivant",
    },
    modeToken: "Au token",
    modeRequest: "Par requête",
    footnote:
      "Les prix se synchronisent toutes les 60 secondes ; null affiche — ; « Gratuit » uniquement lorsque tous les prix effectifs sont à 0.",
  },
};
