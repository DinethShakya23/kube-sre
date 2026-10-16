import { useEffect } from 'react';
import SpotlightCard from './reactbits/SpotlightCard/SpotlightCard.jsx';
import FadeContent from './reactbits/FadeContent/FadeContent.jsx';
import { render } from '../lib/md.js';

export function Card({ className = '', children, ...rest }) {
  return (
    <SpotlightCard
      className={'card ' + className}
      spotlightColor="rgba(109, 125, 255, 0.22)"
      intensity={0.12}
      spotlightSize={280}
      borderGlow={0.5}
      {...rest}
    >
      {children}
    </SpotlightCard>
  );
}

export function Badge({ tone = 'neutral', children }) {
  return <span className={'badge ' + tone}>{children}</span>;
}

export function Page({ title, lede, actions, children }) {
  return (
    <FadeContent trigger="mount" duration={0.45} blur className="page">
      <header className="page-head">
        <div>
          <h1>{title}</h1>
          {lede && <p className="lede">{lede}</p>}
        </div>
        {actions && <div className="page-actions">{actions}</div>}
      </header>
      {children}
    </FadeContent>
  );
}

export function Markdown({ text, className = '' }) {
  return <div className={'prose ' + className} dangerouslySetInnerHTML={{ __html: render(text) }} />;
}

export function Empty({ children }) {
  return <p className="empty">{children}</p>;
}

export function Toast({ notice, onClose }) {
  useEffect(() => {
    if (!notice || notice.sticky) return undefined;
    const t = setTimeout(onClose, 6000);
    return () => clearTimeout(t);
  }, [notice, onClose]);
  if (!notice) return null;
  return (
    <div className={'toast ' + (notice.bad ? 'bad' : 'good')} role="status">
      <span>{notice.text}</span>
      <button type="button" className="toast-x" onClick={onClose} aria-label="Dismiss">
        ×
      </button>
    </div>
  );
}
