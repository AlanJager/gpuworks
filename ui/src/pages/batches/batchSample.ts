import type { BatchEndpoint } from "../../batchApi";

// The sample is a thousand requests built at click time from a seeded PRNG
// and a handful of task templates (multi-step arithmetic, equations, number
// sequences, translation, summaries, stories, code, explanations), half in
// English and half in Chinese. Every prompt asks for a long, reasoned answer
// so the batch keeps a card busy for minutes, while the generator itself
// ships as a few KB. The same seed always gives the same file.

// SAMPLE_LINES is how many requests a sample file holds; an embeddings
// request is cheap, so its sample holds SAMPLE_EMBEDDING_LINES to keep a card
// as busy.
export const SAMPLE_LINES = 1000;
export const SAMPLE_EMBEDDING_LINES = 10000;

// sampleLines is how many requests the sample for the endpoint holds.
export function sampleLines(endpoint: BatchEndpoint): number {
  return endpoint === "/v1/embeddings" ? SAMPLE_EMBEDDING_LINES : SAMPLE_LINES;
}

// SAMPLE_MAX_TOKENS caps each generated answer: room for a reasoning model's
// thinking before the worked solution or essay, while one request stays
// bounded. A deployment's context must hold it plus the prompt.
export const SAMPLE_MAX_TOKENS = 4096;

const SEED = 20261030;

type Rng = () => number;

// mulberry32: a tiny, well-mixed 32-bit PRNG, so a seed fixes the file.
function rng(seed: number): Rng {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const int = (r: Rng, lo: number, hi: number) => lo + Math.floor(r() * (hi - lo + 1));
const pick = <T,>(r: Rng, xs: readonly T[]): T => xs[Math.floor(r() * xs.length)];
const words = (s: string) => s.split("|");
// two draws two different indices below n, for "A and B" in one prompt.
const two = (r: Rng, n: number): [number, number] => {
  const i = int(r, 0, n - 1);
  return [i, (i + int(r, 1, n - 1)) % n];
};

// Word lists, English and Chinese side by side (same index, same meaning).
const goods = { en: words("apples|notebooks|pens|tea cups|cables|batteries|lamps|towels"), zh: words("苹果|笔记本|钢笔|茶杯|数据线|电池|台灯|毛巾") }; // i18n-ignore: sample prompt words
const names = { en: words("Alice|Bob|Chen|Dana|Eli|Fang|Gita|Hiro"), zh: words("小明|小红|老王|阿杰|小林|王芳|李雷|韩梅") }; // i18n-ignore: sample prompt words
const cities = { en: words("Hangzhou|Chengdu|Lisbon|Toronto|Nairobi|Osaka|Lima|Oslo"), zh: words("杭州|成都|里斯本|多伦多|内罗毕|大阪|利马|奥斯陆") }; // i18n-ignore: sample prompt words
const roles = { en: words("lighthouse keeper|young chef|retired pilot|street musician|night-shift nurse|robot repairer"), zh: words("灯塔看守人|年轻厨师|退休飞行员|街头音乐人|夜班护士|机器人修理工") }; // i18n-ignore: sample prompt words
const things = { en: words("an old map|a silent radio|a locked diary|a glowing seed|a broken compass|a letter from the future"), zh: words("一张旧地图|一台不响的收音机|一本上锁的日记|一颗发光的种子|一个坏掉的指南针|一封来自未来的信") }; // i18n-ignore: sample prompt words
const places = { en: words("a mountain village|an empty subway station|a floating market|a desert observatory|a flooded library"), zh: words("山村|空无一人的地铁站|水上市场|沙漠天文台|被水淹没的图书馆") }; // i18n-ignore: sample prompt words
const concepts = { en: words("how a GPU runs many threads at once|why the sky is blue|how vaccines train the immune system|what compound interest is|how a hash table works|why leaves change colour in autumn|how public-key encryption works|what a large language model does"), zh: words("GPU 如何同时运行大量线程|天空为什么是蓝色的|疫苗如何训练免疫系统|什么是复利|哈希表如何工作|秋天树叶为什么变色|公钥加密如何工作|大语言模型在做什么") }; // i18n-ignore: sample prompt words
const audiences = { en: words("a ten-year-old|a new engineer|a busy manager|a history student"), zh: words("十岁的孩子|刚入职的工程师|忙碌的经理|历史系学生") }; // i18n-ignore: sample prompt words
const codeTasks = {
  en: words("returns the n-th Fibonacci number using memoisation|checks whether a string is a palindrome, ignoring punctuation|merges two sorted lists into one sorted list|counts word frequencies in a text and returns the top k|converts a Roman numeral to an integer|finds all primes below n with a sieve|rotates an n×n matrix by 90 degrees|validates a balanced bracket string"),
  zh: words("用记忆化求第 n 个斐波那契数|忽略标点判断字符串是否为回文|把两个有序列表合并成一个有序列表|统计文本词频并返回前 k 个|把罗马数字转换成整数|用筛法找出小于 n 的所有质数|把 n×n 矩阵旋转 90 度|校验括号字符串是否配对"), // i18n-ignore: sample prompt words
};
const langs = words("Python|Go|TypeScript|Rust");
const subjects = { en: words("My neighbour|The engineer|Our teacher|The old fisherman|A small cat|The new intern"), zh: words("我的邻居|那位工程师|我们的老师|老渔夫|一只小猫|新来的实习生") }; // i18n-ignore: sample prompt words
const actions = { en: words("repaired the bicycle|planted three trees|wrote a long letter|cooked dinner for everyone|found a lost wallet|finished the report"), zh: words("修好了自行车|种了三棵树|写了一封长信|给大家做了晚饭|捡到了一个丢失的钱包|完成了报告") }; // i18n-ignore: sample prompt words
const whens = { en: words("yesterday morning|last weekend|before the rain started|after the meeting|late at night"), zh: words("昨天早上|上个周末|下雨之前|会议结束后|深夜") }; // i18n-ignore: sample prompt words

type Lang = "en" | "zh";
type Template = (r: Rng, l: Lang) => string;

// Each template asks for reasoning or long-form text, so answers run to
// hundreds of tokens; the numbers and words drawn make every prompt distinct.
const templates: Template[] = [
  // Multi-step shopping arithmetic with a discount.
  (r, l) => {
    const g = int(r, 0, 7), p = int(r, 3, 99), a = int(r, 2, 40), b = int(r, 2, 40), d = pick(r, [5, 10, 15, 20, 25]);
    const [i, j] = two(r, names[l].length), x = names[l][i], y = names[l][j];
    return l === "en"
      ? `A shop sells ${goods.en[g]} at ${p} yuan each. ${x} buys ${a} and ${y} buys ${b}; ${y} gets ${d}% off. How much do they pay in total, and how much more would they pay without the discount? Reason step by step.`
      : `商店的${goods.zh[g]}每件 ${p} 元。${x}买了 ${a} 件，${y}买了 ${b} 件并享受 ${d}% 折扣。两人一共付多少钱？如果没有折扣要多付多少？请一步一步推理。`; // i18n-ignore: sample prompt
  },
  // Long multiplication with every partial product shown.
  (r, l) => {
    const x = int(r, 1000, 9999), y = int(r, 100, 999);
    return l === "en"
      ? `Compute ${x} × ${y} by long multiplication. Show every partial product, add them up, then check the result by estimating.`
      : `用竖式计算 ${x} × ${y}，写出每一个部分积并相加，最后用估算检验结果。`; // i18n-ignore: sample prompt
  },
  // A 2×2 linear system with an integer solution.
  (r, l) => {
    const x = int(r, -9, 9), y = int(r, -9, 9), a = int(r, 1, 9), b = int(r, 1, 9), c = int(r, 2, 9), d = -int(r, 2, 9);
    const sys = `${a}x + ${b}y = ${a * x + b * y}, ${c}x - ${-d}y = ${c * x + d * y}`;
    return l === "en"
      ? `Solve the system ${sys}. Use elimination, show each step, and substitute the answer back to verify it.`
      : `解方程组：${sys}。用消元法，写出每一步，并把结果代回原方程验证。`; // i18n-ignore: sample prompt
  },
  // Two trains meeting: rates, times and distance.
  (r, l) => {
    const [i, j] = two(r, cities[l].length), c1 = cities[l][i], c2 = cities[l][j];
    const v1 = int(r, 60, 160), v2 = int(r, 60, 160), dist = int(r, 200, 1500), h = int(r, 6, 11), m = pick(r, [0, 15, 30, 45]);
    const t = `${h}:${String(m).padStart(2, "0")}`;
    return l === "en"
      ? `A train leaves ${c1} at ${t} at ${v1} km/h; another leaves ${c2}, ${dist} km away, at the same time at ${v2} km/h, heading toward the first. At what time do they meet and how far from ${c1}? Show the working.`
      : `一列火车 ${t} 从${c1}出发，时速 ${v1} 公里；另一列同时从相距 ${dist} 公里的${c2}出发相向而行，时速 ${v2} 公里。它们几点相遇？相遇点离${c1}多远？请写出计算过程。`; // i18n-ignore: sample prompt
  },
  // Number sequences: find the rule, extend, prove.
  (r, l) => {
    const a = int(r, 1, 30), k = int(r, 2, 9), q = int(r, 2, 4), kind = int(r, 0, 2);
    const seq = Array.from({ length: 6 }, (_, i) => (kind === 0 ? a + k * i : kind === 1 ? a * q ** i : a + k * i * i));
    return l === "en"
      ? `Find the rule behind ${seq.join(", ")}, ... Give the next three terms, a formula for the n-th term, and the sum of the first 20 terms.`
      : `找出数列 ${seq.join("，")}，…… 的规律，写出接下来的三项、第 n 项的通项公式，并求前 20 项之和。`; // i18n-ignore: sample prompt
  },
  // Translation of a generated sentence pair, both directions.
  (r, l) => {
    const s = int(r, 0, 5), v = int(r, 0, 5), w = int(r, 0, 4), c = int(r, 0, cities.en.length - 1);
    return l === "en"
      ? `Translate into Chinese, then explain two word choices you made: "${subjects.en[s]} ${actions.en[v]} in ${cities.en[c]} ${whens.en[w]}, and everyone talked about it for days."`
      : `把下面的句子翻译成英文，并解释你的两处用词选择：“${whens.zh[w]}，${subjects.zh[s]}在${cities.zh[c]}${actions.zh[v]}，大家为此谈论了好几天。”`; // i18n-ignore: sample prompt
  },
  // Summarise a generated monthly report.
  (r, l) => {
    const c = int(r, 0, 7), base = int(r, 100, 900);
    const sales = Array.from({ length: 6 }, () => base + int(r, -80, 120));
    const rows = sales.map((v, i) => (l === "en" ? `month ${i + 1}: ${v} units` : `${i + 1} 月 ${v} 件`)).join(l === "en" ? "; " : "；"); // i18n-ignore: sample prompt
    return l === "en"
      ? `The ${cities.en[c]} store sold ${goods.en[c]} as follows — ${rows}. Summarise the trend in three bullet points, name the best and worst month, and compute the average.`
      : `${cities.zh[c]}门店的${goods.zh[c]}销量如下：${rows}。请用三条要点总结趋势，指出最好和最差的月份，并计算月平均销量。`; // i18n-ignore: sample prompt
  },
  // A short story with drawn elements.
  (r, l) => {
    const i = int(r, 0, 5), j = int(r, 0, 5), k = int(r, 0, 4), n = pick(r, [150, 200, 250, 300]);
    return l === "en"
      ? `Write a ${n}-word story about a ${roles.en[i]} who finds ${things.en[j]} in ${places.en[k]}. End with a twist.`
      : `写一篇约 ${n} 字的小故事：一位${roles.zh[i]}在${places.zh[k]}发现了${things.zh[j]}。结尾要有反转。`; // i18n-ignore: sample prompt
  },
  // Code with tests.
  (r, l) => {
    const task = pick(r, codeTasks[l]), lang = pick(r, langs), n = int(r, 2, 6);
    return l === "en"
      ? `Write a ${lang} function that ${task}. Explain its time complexity and add ${n} test cases.`
      : `用 ${lang} 写一个函数，${task}。说明时间复杂度，并给出 ${n} 个测试用例。`; // i18n-ignore: sample prompt
  },
  // Explain a concept to an audience.
  (r, l) => {
    const c = int(r, 0, 7), a = int(r, 0, 3), n = pick(r, [120, 180, 250]);
    return l === "en"
      ? `Explain ${concepts.en[c]} to ${audiences.en[a]} in about ${n} words, with one everyday analogy and one common misconception.`
      : `用大约 ${n} 字向${audiences.zh[a]}解释“${concepts.zh[c]}”，给出一个生活中的类比和一个常见误解。`; // i18n-ignore: sample prompt
  },
];

// SAMPLE_TASK_TYPES is how many kinds of request a sample mixes.
export const SAMPLE_TASK_TYPES = templates.length;

/**
 * samplePrompts draws `count` distinct prompts: request n uses template
 * n mod the template count, alternating languages, so the kinds and
 * languages spread evenly through the file; a repeat is redrawn.
 */
export function samplePrompts(count = SAMPLE_LINES, seed = SEED): string[] {
  const r = rng(seed);
  const seen = new Set<string>();
  const out: string[] = [];
  for (let n = 0; n < count; n++) {
    const make = templates[n % templates.length];
    const lang: Lang = Math.floor(n / templates.length) % 2 === 0 ? "en" : "zh";
    let p = make(r, lang);
    for (let tries = 0; seen.has(p) && tries < 50; tries++) p = make(r, lang);
    seen.add(p);
    out.push(p);
  }
  return out;
}

/**
 * embeddingInputs draws `count` distinct texts to embed: text n is prompt n
 * with a second prompt drawn after it, redrawn while the pair repeats, since
 * the templates alone give fewer distinct prompts than an embeddings sample
 * holds.
 */
export function embeddingInputs(count = SAMPLE_EMBEDDING_LINES, seed = SEED): string[] {
  const pool = samplePrompts(count, seed);
  const r = rng(seed + 1);
  const seen = new Set<string>();
  return pool.map((first) => {
    let text: string;
    do {
      text = `${first}\n${pick(r, pool)}`;
    } while (seen.has(text));
    seen.add(text);
    return text;
  });
}

const sampleBodies: Record<BatchEndpoint, (model: string, prompt: string) => Record<string, unknown>> = {
  "/v1/chat/completions": (model, prompt) => ({ model, messages: [{ role: "user", content: prompt }], max_tokens: SAMPLE_MAX_TOKENS }),
  "/v1/completions": (model, prompt) => ({ model, prompt, max_tokens: SAMPLE_MAX_TOKENS }),
  "/v1/embeddings": (model, prompt) => ({ model, input: prompt }),
};

/**
 * batchSampleJSONL is a ready-to-upload requests file for the endpoint and
 * model, sampleLines(endpoint) requests by default: one request per line with
 * a unique custom_id (req-0001, ..., zero-padded to the widest number),
 * method POST, the endpoint as url and a body naming the model.
 */
export function batchSampleJSONL(endpoint: BatchEndpoint, model: string, lines = sampleLines(endpoint)): string {
  const width = String(lines).length < 4 ? 4 : String(lines).length;
  const texts = endpoint === "/v1/embeddings" ? embeddingInputs(lines) : samplePrompts(lines);
  return texts.map((prompt, n) => JSON.stringify({
    custom_id: `req-${String(n + 1).padStart(width, "0")}`,
    method: "POST",
    url: endpoint,
    body: sampleBodies[endpoint](model, prompt),
  })).join("\n") + "\n";
}

// sampleFileName names the downloaded sample after its endpoint.
export function sampleFileName(endpoint: BatchEndpoint): string {
  return `batch-sample-${endpoint.replace(/^\/v1\//, "").replace(/\//g, "-")}.jsonl`;
}

/**
 * downloadBatchSample hands the browser the sample as a file to save.
 */
export function downloadBatchSample(endpoint: BatchEndpoint, model: string): void {
  const blob = new Blob([batchSampleJSONL(endpoint, model)], { type: "application/jsonl" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = sampleFileName(endpoint);
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
