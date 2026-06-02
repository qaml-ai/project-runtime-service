import { useState, useMemo, useCallback, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { Download } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Separator } from '@/components/ui/separator';
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip';
import { TooltipProvider } from '@/components/ui/tooltip';
import { FilePreviewContent } from '@/components/chat-file-preview/file-preview-content';
import { getPreviewType } from '@/components/chat-file-preview/file-type-utils';

import '@/styles/globals.css';

declare global {
  interface Window {
    __FILENAME__?: string;
  }
}

// Apply dark mode based on system preference
function applyDarkMode() {
  const mq = window.matchMedia('(prefers-color-scheme: dark)');
  const apply = (dark: boolean) => {
    document.documentElement.classList.toggle('dark', dark);
  };
  apply(mq.matches);
  mq.addEventListener('change', (e) => apply(e.matches));
}

applyDarkMode();

function App() {
  const filename = window.__FILENAME__ ?? '';
  const [notebookViewMode, setNotebookViewMode] = useState<
    'report' | 'notebook'
  >('report');
  const [markdownViewMode, setMarkdownViewMode] = useState<
    'rendered' | 'source'
  >('rendered');

  const previewType = useMemo(
    () => (filename ? getPreviewType(filename) : 'other'),
    [filename]
  );

  const handleDownload = useCallback(() => {
    if (!filename) return;
    const link = document.createElement('a');
    link.href = '/files/' + filename;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    requestAnimationFrame(() => link.remove());
  }, [filename]);

  if (!filename) {
    return (
      <div className="flex h-screen items-center justify-center">
        <p className="text-sm text-muted-foreground">No file to preview</p>
      </div>
    );
  }

  const isNotebook = previewType === 'notebook';
  const isMarkdown = previewType === 'markdown';

  return (
    <div className="flex h-screen flex-col">
      <div className="flex items-center gap-1 border-b border-border px-3 py-1.5">
        {isNotebook && (
          <>
            <Tabs
              value={notebookViewMode}
              onValueChange={(v) => {
                if (v === 'report' || v === 'notebook')
                  setNotebookViewMode(v);
              }}
              className="shrink-0 gap-0"
            >
              <TabsList variant="outline" className="h-7">
                <TabsTrigger value="report" className="h-6 px-3 text-xs">
                  Report
                </TabsTrigger>
                <TabsTrigger value="notebook" className="h-6 px-3 text-xs">
                  Notebook
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <Separator
              orientation="vertical"
              className="mx-1 h-4 data-[orientation=vertical]:self-auto"
            />
          </>
        )}
        {isMarkdown && (
          <>
            <Tabs
              value={markdownViewMode}
              onValueChange={(v) => {
                if (v === 'rendered' || v === 'source')
                  setMarkdownViewMode(v);
              }}
              className="shrink-0 gap-0"
            >
              <TabsList variant="outline" className="h-7">
                <TabsTrigger value="rendered" className="h-6 px-3 text-xs">
                  Rendered
                </TabsTrigger>
                <TabsTrigger value="source" className="h-6 px-3 text-xs">
                  Source
                </TabsTrigger>
              </TabsList>
            </Tabs>
            <Separator
              orientation="vertical"
              className="mx-1 h-4 data-[orientation=vertical]:self-auto"
            />
          </>
        )}
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              onClick={handleDownload}
            >
              <Download className="h-4 w-4" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>Download</TooltipContent>
        </Tooltip>
      </div>

      <div className="flex-1 overflow-hidden">
        <FilePreviewContent
          filename={filename}
          previewUrl={'/files/' + filename}
          layout="panel"
          notebookViewMode={notebookViewMode}
          markdownViewMode={markdownViewMode}
        />
      </div>
    </div>
  );
}

createRoot(document.getElementById('root')!).render(
  <TooltipProvider>
    <App />
  </TooltipProvider>
);
