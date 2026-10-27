import { useEffect, useState } from 'react';
import Aurora from '../src/components/reactbits/Aurora/Aurora.jsx';
import BlurText from '../src/components/reactbits/BlurText/BlurText.jsx';
import CountUp from '../src/components/reactbits/CountUp/CountUp.jsx';
import FadeContent from '../src/components/reactbits/FadeContent/FadeContent.jsx';
import GradientText from '../src/components/reactbits/GradientText/GradientText.jsx';
import ShinyText from '../src/components/reactbits/ShinyText/ShinyText.jsx';
import SpotlightCard from '../src/components/reactbits/SpotlightCard/SpotlightCard.jsx';
import StarBorder from '../src/components/reactbits/StarBorder/StarBorder.jsx';
import TextType from '../src/components/reactbits/TextType/TextType.jsx';
import {
  ArrowIcon,
  BookIcon,
  CheckIcon,
  CopyIcon,
  EyeIcon,
  GaugeIcon,
  GitHubIcon,
  LockIcon,
  Logo,
  PlayIcon,
  RadarIcon,
  SearchIcon,
  ShieldIcon,
  SlidersIcon
} from '../src/components/Icons.jsx';

const GITHUB = 'https://github.com/DinethShakya23/kube-sre';
const DEMO = '/ui/';

const NAV = [
  ['Features', '#features'],
  ['How it works', '#how'],
  ['Safety', '#safety'],
  ['Self-host', '#self-host'],
  ['Demo', '#demo']
];

const FEATURES = [
  {
    icon: SearchIcon,
    title: 'Investigate',
    lede: "Ask what's wrong in plain English and watch the answer stream in, backed by evidence.",
    points: [
      'Live reads through kubectl: pods, events, logs, rollouts and deployments',
      'Metrics and log search from Prometheus and Loki, when you have them',
      '27 built in playbooks that know what to check for each kind of failure',
      'A plan with several steps instead of one lucky guess'
    ]
  },
  {
    icon: RadarIcon,
    title: 'Detect',
    lede: 'Find out something is wrong before you would think to ask.',
    points: [
      "Rule based detectors that cost nothing: no AI call just to notice a crash loop",
      'A live watch over pods and events',
      'A feed of every finding, and when it fired',
      'Describe a failure in plain English and it becomes a detector that watches quietly first'
    ]
  },
  {
    icon: BookIcon,
    title: 'Remember',
    lede: "A record you can check, not just an answer you have to trust.",
    points: [
      'A hash chained audit log you can verify with one command',
      'Replay any investigation from start to finish',
      'Postmortems that only cite what was actually recorded',
      'A digest of everything that happened while you were away'
    ]
  },
  {
    icon: SlidersIcon,
    title: 'Operate',
    lede: 'One binary that fits how you already deploy.',
    points: [
      'SQLite by default, or Postgres when you need more than one box',
      'A CLI for chat, digests, postmortems and replays, plus a systemd service install',
      'API keys, expiring demo keys, or an admin login with a hashed password',
      'An optional public demo mode with spend caps and per visitor limits'
    ]
  }
];

const STEPS = [
  ['A detector fires', 'Rules that cost nothing watch the pod and event streams. One matches: recommender in shop was OOMKilled.'],
  [
    'The playbook loads',
    "Its steps and the evidence to expect go into the model's context: describe the pod, check memory over 30 minutes, read the previous container's logs."
  ],
  ['It reads, it does not guess', 'kubectl, Prometheus and Loki are queried live. The answer only uses what those reads actually returned.'],
  ["You approve, or you don't", 'A suggested fix comes to you as a command with a risk level. Nothing runs on the cluster until you say yes.']
];

const SAFETY = [
  {
    icon: LockIcon,
    title: 'Read only unless you say otherwise',
    body: 'In this demo, the identity kube-sre uses can list, get and watch, and nothing else. Kubernetes itself refuses any write, not just a setting.'
  },
  {
    icon: ShieldIcon,
    title: 'Your okay before every change',
    body: 'A proposed command comes to you with a risk level and a plain summary. You approve or deny it right there in the conversation.'
  },
  {
    icon: GaugeIcon,
    title: 'Brakes that fail closed',
    body: 'A kill switch, a change freeze and a spend cap sit in front of any automatic change, and a bad track record takes away auto fix rights on its own.'
  },
  {
    icon: EyeIcon,
    title: "Can't read is not the same as empty",
    body: "When a read fails, kube-sre says so instead of reporting a quiet cluster. A record it couldn't verify is called unverified, never tampered and never clean."
  }
];

const QUICKSTART = [
  ['kube-sre kind-setup', "a local cluster, if you don't have one"],
  ['kubectl apply -f examples/shop.yaml', 'one healthy app, four broken ones'],
  ['export OPENAI_API_KEY=sk-...', 'any OpenAI compatible key'],
  ['kube-sre init', 'writes ~/.kube-sre/.env and prints an admin key once'],
  ['kube-sre serve', 'then open http://localhost:8000/']
];

function Section({ id, className = '', children }) {
  return (
    <section id={id} className={'s-section ' + className}>
      <FadeContent blur duration={0.8} threshold={0.12} className="s-wrap">
        {children}
      </FadeContent>
    </section>
  );
}

function Heading({ kicker, title, lede }) {
  return (
    <div className="s-heading">
      {kicker && <span className="s-kicker">{kicker}</span>}
      <h2>{title}</h2>
      {lede && <p>{lede}</p>}
    </div>
  );
}

function Card({ className = '', children }) {
  return (
    <SpotlightCard className={'s-card ' + className} spotlightColor="rgba(109, 125, 255, 0.22)" intensity={0.12} spotlightSize={320} borderGlow={0.5}>
      {children}
    </SpotlightCard>
  );
}

function Header() {
  const [scrolled, setScrolled] = useState(false);
  useEffect(() => {
    const on = () => setScrolled(window.scrollY > 12);
    on();
    window.addEventListener('scroll', on, { passive: true });
    return () => window.removeEventListener('scroll', on);
  }, []);
  return (
    <header className={'s-header' + (scrolled ? ' scrolled' : '')}>
      <div className="s-header-in">
        <a href="#top" className="s-brand" aria-label="kube-sre home">
          <Logo size={26} />
          <GradientText colors={['#8f9bff', '#4fe0cb', '#b18cff', '#8f9bff']} animationSpeed={10}>
            kube-sre
          </GradientText>
        </a>
        <nav className="s-nav" aria-label="Sections">
          {NAV.map(([label, href]) => (
            <a key={href} href={href}>
              {label}
            </a>
          ))}
        </nav>
        <div className="s-header-actions">
          <a className="s-icon-link" href={GITHUB} aria-label="kube-sre on GitHub">
            <GitHubIcon />
          </a>
          <a className="s-btn" href={DEMO}>
            Open the demo
          </a>
        </div>
      </div>
    </header>
  );
}

const QUESTION = 'why is checkout crash-looping?';
const TYPE_DELAY = 900;
const TYPE_SPEED = 45;

function Terminal() {
  const [typed, setTyped] = useState(false);
  // TextType reports no completion when it does not loop, so time the answer from the typing speed.
  useEffect(() => {
    const t = setTimeout(() => setTyped(true), TYPE_DELAY + QUESTION.length * TYPE_SPEED + 400);
    return () => clearTimeout(t);
  }, []);
  return (
    <div className="s-term" aria-label="An example conversation">
      <div className="s-term-bar">
        <span className="s-dots" aria-hidden="true">
          <i />
          <i />
          <i />
        </span>
        <span>kube-sre chat</span>
      </div>
      <div className="s-term-body">
        <div className="s-term-q">
          <span className="s-prompt">›</span>
          <TextType
            as="span"
            text={QUESTION}
            typingSpeed={TYPE_SPEED}
            initialDelay={TYPE_DELAY}
            loop={false}
            showCursor={!typed}
            cursorCharacter="▍"
          />
        </div>
        <div className={'s-term-a' + (typed ? ' on' : '')}>
          <span className="s-term-tag">answer</span>
          checkout exits with code 1: connection refused on <code>db.shop.svc:5432</code>. It restarted 6 times in 4 minutes. Other pods can reach the
          database fine, so this looks like checkout's own config.
          <div className="s-term-cite">
            Evidence: <code>kubectl logs checkout --previous</code>, <code>kubectl get events -n shop</code>
          </div>
        </div>
      </div>
    </div>
  );
}

function Hero() {
  return (
    <section id="top" className="s-hero">
      <div className="s-hero-bg" aria-hidden="true">
        <Aurora colorStops={['#3346ff', '#28c7b2', '#6a2cff']} amplitude={0.9} blend={0.55} speed={0.5} opacity={0.55} mouseInteraction={false} />
      </div>
      <div className="s-hero-in">
        <span className="s-pill">
          <span className="s-pill-dot" aria-hidden="true" />
          <ShinyText text="Open source · Self-hosted · Read only by default" color="#9aa3ba" shineColor="#ffffff" speed={3} />
        </span>
        <h1 className="s-title">
          <BlurText text="An AI SRE for Kubernetes" className="s-title-line" delay={70} animateBy="words" direction="top" trigger="load" />
          <GradientText colors={['#8f9bff', '#4fe0cb', '#b18cff', '#8f9bff']} animationSpeed={8} className="s-title-grad">
            that shows its work
          </GradientText>
        </h1>
        <p className="s-lede">Ask it what's wrong. It reads your cluster, shows you the evidence it found, and never changes anything without your okay.</p>
        <div className="s-ctas">
          <StarBorder as="a" href={DEMO} className="s-star" color="#7c8cff" trailColor="#3fd9c4" radius={12} duration={5} backgroundColor="#151a2e" textColor="#ffffff">
            Try the live demo <ArrowIcon />
          </StarBorder>
          <a className="s-btn ghost" href="#how">
            See how it works
          </a>
        </div>
        <Terminal />
      </div>
    </section>
  );
}

function Stats() {
  const items = [
    [27, 'built in playbooks'],
    [20, 'detectors that never call the model'],
    [1, 'binary, web UI included'],
    [0, 'changes without your okay']
  ];
  return (
    <div className="s-stats">
      {items.map(([n, label]) => (
        <div key={label} className="s-stat">
          <span className="s-stat-n">
            <CountUp to={n} duration={1.6} />
          </span>
          <span className="s-stat-l">{label}</span>
        </div>
      ))}
    </div>
  );
}

function Features() {
  return (
    <Section id="features">
      <Heading title="What it actually does" lede="No autonomous magic. Just quick reads backed by evidence, and a record you can check." />
      <div className="s-grid2">
        {FEATURES.map(({ icon: Icon, title, lede, points }) => (
          <Card key={title}>
            <span className="s-ico">
              <Icon />
            </span>
            <h3>{title}</h3>
            <p className="s-muted">{lede}</p>
            <ul className="s-checks">
              {points.map(p => (
                <li key={p}>
                  <CheckIcon />
                  <span>{p}</span>
                </li>
              ))}
            </ul>
          </Card>
        ))}
      </div>
    </Section>
  );
}

function Walkthrough() {
  return (
    <Section id="walkthrough">
      <Heading
        kicker="Product walkthrough"
        title="See it in action"
        lede="A real investigation from start to finish: the question, the reads it makes, the evidence it cites, and the approval prompt before anything changes."
      />
      <div className="s-video">
        <PlayIcon />
        <span>Coming soon</span>
      </div>
    </Section>
  );
}

function How() {
  return (
    <Section id="how" className="s-band">
      <Heading title="How a diagnosis actually happens" lede="The same four steps, whether you asked in chat or a detector noticed first." />
      <ol className="s-steps">
        {STEPS.map(([title, body], i) => (
          <li key={title}>
            <Card className="s-step">
              <span className="s-step-n">0{i + 1}</span>
              <h3>{title}</h3>
              <p className="s-muted">{body}</p>
            </Card>
          </li>
        ))}
      </ol>
      <div className="s-note">
        <SlidersIcon />
        <p>
          <b>Autonomy is a dial, not a switch.</b> You set the level per namespace, from just watching, to looking into it and reporting back, to
          proposing a fix. Even the highest level only acts on its own for failure types you explicitly allow. Never for a predicted failure, and never
          past a kill switch, a change freeze or a spend cap.
        </p>
      </div>
    </Section>
  );
}

function Safety() {
  return (
    <Section id="safety">
      <Heading
        title="Safety is the feature"
        lede="A wrong diagnosis costs you an afternoon. A made up one, or a change nobody approved, is a real incident. So these are rules, not preferences."
      />
      <div className="s-grid2">
        {SAFETY.map(({ icon: Icon, title, body }) => (
          <Card key={title} className="s-row">
            <span className="s-ico">
              <Icon />
            </span>
            <div>
              <h3>{title}</h3>
              <p className="s-muted">{body}</p>
            </div>
          </Card>
        ))}
      </div>
    </Section>
  );
}

function SelfHost() {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(QUICKSTART.map(([c]) => c).join('\n'));
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // Clipboard access can be refused; the commands are still on screen.
    }
  };
  return (
    <Section id="self-host" className="s-band">
      <div className="s-split">
        <div>
          <h2>Run it on your own cluster</h2>
          <p className="s-muted">
            One Go binary with the web UI built in, and SQLite by default. Point it at a read only service account and it can't change anything, even
            if it wanted to.
          </p>
          <p className="s-muted small">
            You need Docker, <code>kubectl</code>, and an API key for any OpenAI compatible model.
          </p>
          <a className="s-btn ghost" href={GITHUB}>
            <GitHubIcon /> View on GitHub
          </a>
        </div>
        <div className="s-code">
          <div className="s-code-bar">
            <span>quickstart</span>
            <button type="button" className="s-copy" onClick={copy}>
              {copied ? <CheckIcon /> : <CopyIcon />} {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
          <pre>
            {QUICKSTART.map(([cmd, note]) => (
              <div key={cmd} className="s-code-line">
                <span>
                  <span className="s-prompt">$</span> {cmd}
                </span>
                <span className="s-code-note"># {note}</span>
              </div>
            ))}
          </pre>
        </div>
      </div>
    </Section>
  );
}

function Demo() {
  return (
    <section id="demo" className="s-demo">
      <div className="s-demo-bg" aria-hidden="true">
        <Aurora colorStops={['#28c7b2', '#3346ff', '#6a2cff']} amplitude={0.8} blend={0.6} speed={0.45} opacity={0.45} direction="bottom" mouseInteraction={false} />
      </div>
      <FadeContent blur duration={0.8} threshold={0.15} className="s-wrap s-demo-in">
        <h2>Try it before you install anything</h2>
        <p className="s-muted">
          A hosted copy is open to everyone. It watches a small sandbox cluster with a handful of apps broken on purpose: a crash loop, a bad image, a
          probe that never passes, and an out of memory kill. Ask it what's wrong with any of them. Nothing to install.
        </p>
        <StarBorder as="a" href={DEMO} className="s-star" color="#3fd9c4" trailColor="#7c8cff" radius={12} duration={5} backgroundColor="#151a2e" textColor="#ffffff">
          Open the demo <ArrowIcon />
        </StarBorder>
        <ul className="s-limits">
          <li>20 questions per visitor, per hour</li>
          <li>One shared daily budget for the whole demo</li>
          <li>Read only, so nothing you ask can change the cluster</li>
        </ul>
      </FadeContent>
    </section>
  );
}

function Footer() {
  return (
    <footer className="s-footer">
      <div className="s-footer-in">
        <span>kube-sre, a Go re-implementation of an SRE agent for Kubernetes.</span>
        <a href={GITHUB} className="s-icon-link">
          <GitHubIcon /> GitHub
        </a>
      </div>
    </footer>
  );
}

export default function Site() {
  return (
    <>
      <Header />
      <main>
        <Hero />
        <Stats />
        <Features />
        <Walkthrough />
        <How />
        <Safety />
        <SelfHost />
        <Demo />
      </main>
      <Footer />
    </>
  );
}
