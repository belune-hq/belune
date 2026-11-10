'use client';

import { useEffect, useId, useRef, useState } from 'react';

/**
 * Client-side Mermaid renderer. The site is a static export, so diagrams render
 * in the browser rather than at build time. The theme follows the docs' light/
 * dark toggle by watching the `.dark` class Fumadocs sets on <html>.
 *
 * mermaid is dynamically imported so its (large) bundle only loads on pages that
 * actually use a diagram.
 */
export function Mermaid({ chart }: { chart: string }) {
  const baseId = useId().replace(/[^a-zA-Z]/g, '');
  const seq = useRef(0);
  const [svg, setSvg] = useState('');
  const [isDark, setIsDark] = useState(false);

  // Track the docs theme so the diagram re-renders with matching mermaid colors.
  useEffect(() => {
    const root = document.documentElement;
    const sync = () => setIsDark(root.classList.contains('dark'));
    sync();
    const observer = new MutationObserver(sync);
    observer.observe(root, { attributes: true, attributeFilter: ['class'] });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    let active = true;
    void (async () => {
      const { default: mermaid } = await import('mermaid');
      mermaid.initialize({
        startOnLoad: false,
        theme: isDark ? 'dark' : 'default',
        securityLevel: 'loose',
        fontFamily: 'inherit',
        // mermaid 12 changed two defaults: ELK replaced dagre as the layout
        // engine, and the 'neo' look replaced 'classic'. Both are pinned here
        // because the change is invisible to CI — the diagram renders in the
        // browser on one page, so a build that goes green says nothing about
        // it. Under ELK the 'App Domains' edge label lands on the subgraph
        // border and the Belune node's label wraps to five lines.
        layout: 'dagre',
        look: 'classic',
      });
      // Fresh id each render — mermaid injects a temp node keyed by id, and a
      // repeated id on theme change would collide.
      const { svg } = await mermaid.render(`mmd-${baseId}-${seq.current++}`, chart);
      if (active) setSvg(svg);
    })();
    return () => {
      active = false;
    };
  }, [chart, isDark, baseId]);

  // Safe: `chart` is an author-written compile-time constant from the MDX (never
  // user input), and `svg` is mermaid's own rendered output — no untrusted path.
  return (
    <div
      role="img"
      className="my-6 flex justify-center [&_svg]:h-auto [&_svg]:max-w-full"
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
