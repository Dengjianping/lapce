package plugin

import (
	"context"
	"io/ioutil"
	"log"
	"os"
	"runtime/debug"

	"github.com/dzhou121/crane/fuzzy"
	"github.com/dzhou121/crane/lsp"
	"github.com/dzhou121/crane/plugin"
	"github.com/dzhou121/crane/utils"
	"github.com/sourcegraph/jsonrpc2"
)

// Plugin is
type Plugin struct {
	plugin          *plugin.Plugin
	lsp             map[string]*lsp.Client
	views           map[string]*plugin.View
	conns           map[string]*jsonrpc2.Conn
	server          *Server
	completionItems []*lsp.CompletionItem
	completionShown bool
}

// NewPlugin is
func NewPlugin() *Plugin {
	p := &Plugin{
		plugin: plugin.NewPlugin(),
		lsp:    map[string]*lsp.Client{},
		views:  map[string]*plugin.View{},
		conns:  map[string]*jsonrpc2.Conn{},
	}
	p.plugin.SetHandleFunc(p.handle)
	return p
}

// Run is
func (p *Plugin) Run() {
	file, err := os.OpenFile("/tmp/log", os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatal(err)
	}
	log.SetOutput(file)
	log.Println("now start to run")
	go func() {
		server, err := newServer(p)
		if err != nil {
			return
		}
		server.run()
	}()
	<-p.plugin.Stop
}

func (p *Plugin) handle(req interface{}) interface{} {
	defer func() {
		if r := recover(); r != nil {
			log.Println("handle error", r, string(debug.Stack()))
		}
	}()
	switch r := req.(type) {
	case *plugin.Initialization:
		for _, buf := range r.BufferInfo {
			viewID := buf.Views[0]
			view := &plugin.View{
				ID:     viewID,
				Path:   buf.Path,
				Syntax: buf.Syntax,
				LineCache: &plugin.LineCache{
					ViewID: viewID,
				},
			}
			p.views[viewID] = view
			lspClient, ok := p.lsp[buf.Syntax]
			if !ok {
				var err error
				lspClient, err = lsp.NewClient()
				if err != nil {
					return nil
				}
				dir, err := os.Getwd()
				if err != nil {
					return nil
				}
				err = lspClient.Initialize(dir)
				if err != nil {
					return nil
				}
				p.lsp[buf.Syntax] = lspClient
			}

			content, err := ioutil.ReadFile(buf.Path)
			if err != nil {
				return nil
			}
			log.Println("now set raw content")
			view.SetRaw(content)
			log.Println("set raw content done", buf.Path)
			err = lspClient.DidOpen(buf.Path, string(content))
			log.Println("did open done")
			if err != nil {
				return nil
			}
		}
	case *plugin.Update:
		view := p.views[r.ViewID]
		startRow, startCol, endRow, endCol, text, deletedText, changed := view.ApplyUpdate(r)
		log.Println(startRow, startCol, endRow, endCol, text, deletedText, changed)
		if !changed {
			return 0
		}
		didChange := &lsp.DidChangeParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{
				URI: "file://" + view.Path,
			},
			ContentChanges: []*lsp.ContentChange{
				&lsp.ContentChange{
					Range: &lsp.Range{
						Start: &lsp.Position{
							Line:      startRow,
							Character: startCol,
						},
						End: &lsp.Position{
							Line:      endRow,
							Character: endCol,
						},
					},
					Text: text,
				},
			},
		}
		lspClient := p.lsp[view.Syntax]
		lspClient.DidChange(didChange)
		p.complete(lspClient, view, text, deletedText, startRow, startCol)
	}
	return 0
}

func (p *Plugin) complete(lspClient *lsp.Client, view *plugin.View, text string, deletedText string, startRow int, startCol int) {
	log.Println("new text is", text)
	log.Println("deleted text is", deletedText)
	runes := []rune(text)
	deletedRunes := []rune(deletedText)

	reset := false
	if len(runes) > 1 {
		reset = true
	}
	if !reset {
		for _, r := range runes {
			if utils.UtfClass(r) != 2 {
				reset = true
				break
			}
		}
	}
	if !reset {
		for _, r := range deletedRunes {
			if utils.UtfClass(r) != 2 {
				reset = true
				break
			}
		}
	}
	if reset && len(p.completionItems) > 0 {
		p.completionItems = []*lsp.CompletionItem{}
	}

	if len(runes) > 1 {
		p.notifyCompletion(p.completionItems)
		return
	}

	if len(runes) > 0 {
		lastRune := runes[len(runes)-1]
		if lastRune != '.' && utils.UtfClass(runes[len(runes)-1]) != 2 {
			p.notifyCompletion(p.completionItems)
			return
		}
	}

	items := p.getCompletionItems(lspClient, view, text, startRow, startCol)
	p.notifyCompletion(items)
}

func (p *Plugin) notifyCompletion(items []*lsp.CompletionItem) {
	if len(items) > 0 {
		p.completionShown = true
	} else {
		p.completionShown = false
	}
	for _, conn := range p.conns {
		conn.Notify(context.Background(), "completion", items)
	}
}

func (p *Plugin) notifyCompletionPos(pos *lsp.Position) {
	for _, conn := range p.conns {
		conn.Notify(context.Background(), "completion_pos", pos)
	}
}

func (p *Plugin) getCompletionItems(lspClient *lsp.Client, view *plugin.View, text string, startRow int, startCol int) []*lsp.CompletionItem {
	if len(p.completionItems) > 0 {
		if text == "" {
			startCol--
		}
		_, word := p.getWord(view, startRow, startCol)
		log.Println("word is", string(word))
		return p.matchCompletionItems(p.completionItems, word)
	}

	word := []rune{}
	if len(text) == 1 {
		if text == "." {
			startCol++
		} else if utils.UtfClass([]rune(text)[0]) == 2 {
			startCol, word = p.getWord(view, startRow, startCol)
		}
	} else if text == "" {
		// startCol, word = p.getWord(view, startRow, startCol-1)
		return p.completionItems
	}
	pos := lsp.Position{
		Line:      startRow,
		Character: startCol,
	}
	params := &lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: "file://" + view.Path,
		},
		Position: pos,
	}
	resp, err := lspClient.Completion(params)
	if err != nil {
		return []*lsp.CompletionItem{}
	}
	p.notifyCompletionPos(&pos)
	p.completionItems = resp.Items
	return p.matchCompletionItems(p.completionItems, word)
}

func (p *Plugin) matchCompletionItems(items []*lsp.CompletionItem, word []rune) []*lsp.CompletionItem {
	if len(word) == 0 {
		for _, item := range items {
			if len(item.Matches) > 0 {
				item.Matches = []int{}
			}
		}
		return items
	}
	matchItems := []*lsp.CompletionItem{}
	for _, item := range items {
		score, matches := fuzzy.MatchScore([]rune(item.InsertText), word)
		if score > -1 {
			i := 0
			for i = 0; i < len(matchItems); i++ {
				matchItem := matchItems[i]
				if score < matchItem.Score {
					break
				}
			}
			item.Score = score
			item.Matches = matches
			matchItems = append(matchItems, nil)
			copy(matchItems[i+1:], matchItems[i:])
			matchItems[i] = item
		}
	}
	return matchItems
}

func (p *Plugin) getWord(view *plugin.View, row, col int) (int, []rune) {
	line := view.LineCache.Lines[row]
	runes := []rune(line.Text)
	word := []rune{}
	for i := col; i >= 0; i-- {
		if utils.UtfClass(runes[i]) != 2 {
			return i + 1, word
		}
		word = append([]rune{runes[i]}, word...)
	}
	return 0, word
}
