const jsonLd = {
	"@context": "https://schema.org",
	"@type": "WebPage",
	url: "https://www.getkoutaku.ai/koutaku/docs",
	name: "Koutaku Documentation",
	description:
		"Comprehensive documentation for Koutaku's end-to-end platform for AI simulation, evaluation, and observability. Learn how to build, evaluate, and monitor GenAI workflows at scale.",
	publisher: {
		"@type": "Organization",
		name: "Koutaku",
		url: "https://www.getkoutaku.ai/koutaku",
		logo: {
			"@type": "ImageObject",
			url: "https://koutaku.getkoutaku.ai/logo-full.svg",
			width: 300,
			height: 60,
		},
		sameAs: ["https://twitter.com/getkoutakuai", "https://www.linkedin.com/company/koutaku-ai", "https://www.youtube.com/@getkoutakuai"],
	},
	mainEntity: {
		"@type": "TechArticle",
		name: "Koutaku Documentation",
		url: "https://www.getkoutaku.ai/koutaku",
		headline: "Koutaku Docs",
		description:
			"Koutaku is the fastest LLM gateway in the market, 90x faster than LiteLLM (P99 latency).",
		inLanguage: "en",
	},
};

function injectJsonLd() {
	const script = document.createElement("script");
	script.type = "application/ld+json";
	script.text = JSON.stringify(jsonLd);

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", () => {
			document.head.appendChild(script);
		});
	} else {
		document.head.appendChild(script);
	}

	return () => {
		if (script.parentNode) {
			script.parentNode.removeChild(script);
		}
	};
}

// Call the function to inject JSON-LD
const cleanup = injectJsonLd();

// Cleanup when needed
// cleanup()