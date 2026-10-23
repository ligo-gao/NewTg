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
      { label: "Modèles", href: "#models" },
      { label: "Tarification", href: "/pricing" },
      { label: "Documentation", href: "/docs" },
      { label: "Console", href: "/dashboard" },
    ],
    login: "Se connecter",
    cta: "Commencer gratuitement",
    openMenu: "Ouvrir le menu de navigation",
    closeMenu: "Fermer le menu de navigation",
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
    ariaLabel: "Envergure de la plateforme",
    items: [
      { value: "100+", label: "modèles chez 6 fournisseurs" },
      { value: "99.9%", label: "disponibilité du service" },
      { value: "10ms", label: "routage intelligent" },
      { value: "10,000+", label: "développeurs" },
    ],
    note: "Les chiffres sont ceux de la version de production.",
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
    unavailable: "Données de performance temporairement indisponibles",
    retry: "Réessayer",
    empty: "Aucun modèle à afficher.",
    footnote:
      "Moyennes issues d'appels réels sur 7 jours. La latence est le temps jusqu'au premier token (TTFT) pour les appels en streaming ; la vitesse de sortie est estimée à partir des tokens de sortie totaux sur la latence totale. Les modèles avec moins de 20 échantillons sont signalés. Les données manquantes sont affichées —.",
  },

  closing: {
    title: "Prêt à connecter tous les modèles ?",
    cta: "Commencer gratuitement",
    note: "Sans carte bancaire. Crédits d'essai à l'inscription.",
  },

  footer: {
    tagline:
      "Une API pour tous les grands modèles. Accès unifié, routage intelligent, bascule automatique et facturation.",
    columns: [
      {
        title: "Produit",
        links: [
          { label: "Modèles", href: "#models" },
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
};
