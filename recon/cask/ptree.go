/*
   conflux - Distributed database synchronization library
	Based on the algorithm described in
		"Set Reconciliation with Nearly Optimal	Communication Complexity",
			Yaron Minsky, Ari Trachtenberg, and Richard Zippel, 2004.

   Copyright (C) 2012  Casey Marshall <casey.marshall@gmail.com>

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package cask

import (
	"bytes"
	"fmt"
	. "github.com/cmars/conflux"
	. "github.com/cmars/conflux/recon"
	"encoding/gob"
	"code.google.com/p/gocask"
	"net"
	"path/filepath"
	"os"
	"errors"
)

type client struct {
	settingsPath string
	ptreePath string
}

func NewPeer(basepath string) (p *Peer, err error) {
	client, err := newClient(basepath)
	if err != nil {
		return nil, err
	}
	settings, err := newSettings(client)
	if err != nil {
		return nil, err
	}
	settings.Init()
	tree, err := newPrefixTree(settings)
	if err != nil {
		return nil, err
	}
	return &Peer{
		RecoverChan: make(RecoverChan),
		Settings:    settings,
		PrefixTree:  tree}, nil
}

func newClient(basepath string) (c *client, err error) {
	c = &client{}
	c.settingsPath = filepath.Join(basepath, "settings")
	c.ptreePath = filepath.Join(basepath, "ptree")
	var fi os.FileInfo
	for _, path := range []string{ c.settingsPath, c.ptreePath } {
		fi, err = os.Stat(path)
		if os.IsNotExist(err) {
			err = os.MkdirAll(path, os.FileMode(0755))
			if err != nil {
				return
			}
		} else if !fi.IsDir() {
			err = errors.New(fmt.Sprintf("Not a directory: %s", path))
			return
		}
	}
	return
}

type settings struct {
	*client
	settingsStore *gocask.Gocask
	*config
}

type config struct {
	Version                     string
	LogName                     string
	HttpPort                    int
	ReconPort                   int
	Partners                    []string
	Filters                     []string
	ThreshMult                  int
	BitQuantum                  int
	MBar                        int
	SplitThreshold              int
	JoinThreshold               int
	NumSamples                  int
	GossipIntervalSecs          int
	MaxOutstandingReconRequests int
}

func newSettings(c *client) (s *settings, err error) {
	s = &settings{client: c}
	s.settingsStore, err = gocask.NewGocask(c.settingsPath)
	return s, err
}

func (s *settings) Init() {
	raw, err := s.settingsStore.Get("")
	if err == gocask.ErrKeyNotFound {
		// Set defaults
		s.config = &config{
			Version:                     "experimental",
			HttpPort:                    11371,
			ReconPort:                   11370,
			ThreshMult:                  DefaultThreshMult,
			BitQuantum:                  DefaultBitQuantum,
			MBar:                        DefaultMBar,
			GossipIntervalSecs:          60,
			MaxOutstandingReconRequests: 100}
		buf := bytes.NewBuffer(nil)
		enc := gob.NewEncoder(buf)
		err = enc.Encode(s.config)
		if err != nil {
			panic(err)
		}
		s.settingsStore.Put("", buf.Bytes())
	} else {
		s.config = &config{}
		dec := gob.NewDecoder(bytes.NewBuffer(raw))
		err = dec.Decode(s.config)
		if err != nil {
			panic(err)
		}
	}
	s.config.SplitThreshold = s.config.ThreshMult * s.config.MBar
	s.config.JoinThreshold = s.config.SplitThreshold / 2
	s.config.NumSamples = s.config.MBar + 1
}

func (s *settings) update() {
	buf := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(buf)
	err := enc.Encode(s.config)
	if err != nil {
		panic(err)
	}
	s.settingsStore.Put("", buf.Bytes())
}

func (s *settings) Version() string {
	return s.config.Version
}

func (s *settings) LogName() string {
	return s.config.LogName
}

func (s *settings) HttpPort() int {
	return s.config.HttpPort
}

func (s *settings) ReconPort() int {
	return s.config.ReconPort
}

func (s *settings) Partners() (addrs []net.Addr) {
	for _, partner := range s.config.Partners {
		addr, err := net.ResolveTCPAddr("tcp", partner)
		if err != nil {
			panic(err)
		}
		addrs = append(addrs, addr)
	}
	return
}

func (s *settings) Filters() []string {
	return s.config.Filters
}

func (s *settings) ThreshMult() int {
	return s.config.ThreshMult
}

func (s *settings) BitQuantum() int {
	return s.config.BitQuantum
}

func (s *settings) MBar() int {
	return s.config.MBar
}

func (s *settings) SplitThreshold() int {
	return s.config.SplitThreshold
}

func (s *settings) JoinThreshold() int {
	return s.config.JoinThreshold
}

func (s *settings) NumSamples() int {
	return s.config.NumSamples
}

func (s *settings) GossipIntervalSecs() int {
	return s.config.GossipIntervalSecs
}

func (s *settings) MaxOutstandingReconRequests() int {
	return s.config.MaxOutstandingReconRequests
}

type prefixTree struct {
	*settings
	ptree  *gocask.Gocask
	points []*Zp
}

func newPrefixTree(s *settings) (tree *prefixTree, err error) {
	tree = &prefixTree{settings: s}
	tree.points = Zpoints(P_SKS, tree.NumSamples())
	tree.ptree, err = gocask.NewGocask(tree.ptreePath)
	if err != nil {
		return
	}
	err = tree.ensureRoot()
	if err != nil {
		return
	}
	return tree, err
}

func (t *prefixTree) ensureRoot() error {
	_, err := t.Root()
	if err != gocask.ErrKeyNotFound {
		return err
	}
	_, err = t.newChildNode(nil, 0)
	return err
}

func (t *prefixTree) Points() []*Zp { return t.points }

func (t *prefixTree) Root() (PrefixNode, error) {
	return t.Node(NewBitstring(0))
}

func (t *prefixTree) Node(bs *Bitstring) (node PrefixNode, err error) {
	key := bytes.NewBuffer(nil)
	err = WriteBitstring(key, bs)
	if err != nil {
		return
	}
	ndRaw, err := t.ptree.Get(string(key.Bytes()))
	if err != nil {
		return
	}
	dec := gob.NewDecoder(bytes.NewBuffer(ndRaw))
	nd := new(nodeData)
	err = dec.Decode(nd)
	if err != nil {
		return
	}
	return t.loadNode(nd)
}

func (t *prefixTree) Insert(z *Zp) error {
	bs := NewBitstring(P_SKS.BitLen())
	bs.SetBytes(ReverseBytes(z.Bytes()))
	root, err := t.Root()
	if err != nil {
		return err
	}
	return root.(*prefixNode).insert(z, AddElementArray(t, z), bs, 0)
}

func (t *prefixTree) Remove(z *Zp) error {
	bs := NewBitstring(P_SKS.BitLen())
	bs.SetBytes(ReverseBytes(z.Bytes()))
	root, err := t.Root()
	if err != nil {
		return err
	}
	return root.(*prefixNode).remove(z, DelElementArray(t, z), bs, 0)
}

func (t *prefixTree) newChildNode(parent *prefixNode, childIndex int) (*prefixNode, error) {
	n := &prefixNode{prefixTree: t}
	if parent != nil {
		parentKey := parent.Key()
		n.key = NewBitstring(parentKey.BitLen() + t.BitQuantum())
		n.key.SetBytes(parentKey.Bytes())
		for j := 0; j < parent.BitQuantum(); j++ {
			if (childIndex>>uint(j))&0x1 == 1 {
				n.key.Set(parentKey.BitLen() + j)
			} else {
				n.key.Unset(parentKey.BitLen() + j)
			}
		}
	} else {
		n.key = NewBitstring(0)
	}
	n.svalues = make([]*Zp, t.NumSamples())
	for i := 0; i < len(n.svalues); i++ {
		n.svalues[i] = Zi(P_SKS, 1)
	}
	err := t.saveNode(n)
	return n, err
}

func (t *prefixTree) loadNode(nd *nodeData) (n *prefixNode, err error) {
	n = &prefixNode{prefixTree: t}
	n.key, err = ReadBitstring(bytes.NewBuffer(nd.KeyBuf))
	if err != nil {
		return
	}
	n.numElements = nd.NumElements
	n.svalues, err = ReadZZarray(bytes.NewBuffer(nd.SvaluesBuf))
	if err != nil {
		return
	}
	n.elements, err = ReadZZarray(bytes.NewBuffer(nd.ElementsBuf))
	if err != nil {
		return
	}
	n.childKeys = nd.ChildKeys
	return
}

func (t *prefixTree) saveNode(n *prefixNode) (err error) {
	nd := &nodeData{}
	var out *bytes.Buffer
	// Write key
	out = bytes.NewBuffer(nil)
	err = WriteBitstring(out, n.key)
	if err != nil {
		return
	}
	nd.KeyBuf = out.Bytes()
	// Write sample values
	out = bytes.NewBuffer(nil)
	err = WriteZZarray(out, n.svalues)
	if err != nil {
		return
	}
	nd.SvaluesBuf = out.Bytes()
	// Write elements
	out = bytes.NewBuffer(nil)
	err = WriteZZarray(out, n.elements)
	nd.ElementsBuf = out.Bytes()
	nd.NumElements = n.numElements
	nd.ChildKeys = n.childKeys
	ndBuf := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(ndBuf)
	err = enc.Encode(nd)
	if err != nil {
		return
	}
	err = t.ptree.Put(string(nd.KeyBuf), ndBuf.Bytes())
	return
}

type nodeData struct {
	KeyBuf []byte
	NumElements int
	SvaluesBuf     []byte
	ElementsBuf    []byte
	ChildKeys   []int
}

type prefixNode struct {
	*prefixTree
	key         *Bitstring
	numElements int
	svalues     []*Zp
	elements    []*Zp
	childKeys   []int
}

func (n *prefixNode) IsLeaf() bool {
	return len(n.childKeys) == 0
}

func (n *prefixNode) Children() (result []PrefixNode) {
	key := n.Key()
	for _, i := range n.childKeys {
		childKey := NewBitstring(key.BitLen()+n.BitQuantum())
		childKey.SetBytes(key.Bytes())
		for j := 0; j < n.BitQuantum(); j++ {
			if (i>>uint(j))&0x1 == 1 {
				childKey.Set(key.BitLen() + j)
			} else {
				childKey.Unset(key.BitLen() + j)
			}
		}
		child, err := n.Node(childKey)
		if err != nil {
			panic(fmt.Sprintf("Children failed on child#%v: %v", i, err))
		}
		result = append(result, child)
	}
	return
}

func (n *prefixNode) Elements() []*Zp {
	return n.elements
}

func (n *prefixNode) Size() int { return n.numElements }

func (n *prefixNode) SValues() []*Zp {
	return n.svalues
}

func (n *prefixNode) Key() *Bitstring {
	return n.key
}

func (n *prefixNode) Parent() (PrefixNode, bool) {
	if n.key.BitLen() == 0 {
		return nil, false
	}
	parentKey := NewBitstring(n.key.BitLen() - n.BitQuantum())
	parentKey.SetBytes(n.key.Bytes())
	parent, err := n.Node(parentKey)
	if err != nil {
		panic(fmt.Sprintf("Failed to get parent: %v", err))
	}
	return parent, true
}

func (n *prefixNode) insert(z *Zp, marray []*Zp, bs *Bitstring, depth int) (err error) {
	n.updateSvalues(z, marray)
	n.numElements++
	if n.IsLeaf() {
		if len(n.elements) > n.SplitThreshold() {
			err = n.split(depth)
			if err != nil {
				return
			}
		} else {
			n.elements = append(n.elements, z)
			n.saveNode(n)
			return
		}
	}
	n.saveNode(n)
	child := NextChild(n, bs, depth).(*prefixNode)
	return child.insert(z, marray, bs, depth+1)
}

func (n *prefixNode) split(depth int) (err error) {
	// Create child nodes
	numChildren := 1 << uint(n.BitQuantum())
	for i := 0; i < numChildren; i++ {
		// Create new empty child node
		_, err = n.newChildNode(n, i)
		if err != nil {
			return
		}
		n.childKeys = append(n.childKeys, i)
	}
	// Move elements into child nodes
	for _, element := range n.elements {
		bs := NewBitstring(P_SKS.BitLen())
		bs.SetBytes(ReverseBytes(element.Bytes()))
		child := NextChild(n, bs, depth).(*prefixNode)
		child.insert(element, AddElementArray(n.prefixTree, element), bs, depth+1)
	}
	n.elements = nil
	return
}

func (n *prefixNode) updateSvalues(z *Zp, marray []*Zp) {
	if len(marray) != len(n.points) {
		panic("Inconsistent NumSamples size")
	}
	for i := 0; i < len(marray); i++ {
		n.svalues[i] = Z(z.P).Mul(n.svalues[i], marray[i])
	}
}

func (n *prefixNode) remove(z *Zp, marray []*Zp, bs *Bitstring, depth int) error {
	n.updateSvalues(z, marray)
	n.numElements--
	if !n.IsLeaf() {
		if n.numElements <= n.JoinThreshold() {
			n.join()
		} else {
			n.saveNode(n)
			child := NextChild(n, bs, depth).(*prefixNode)
			return child.remove(z, marray, bs, depth+1)
		}
	}
	n.elements = withRemoved(n.elements, z)
	n.saveNode(n)
	return nil
}

func (n *prefixNode) join() {
	for _, child := range n.Children() {
		n.elements = append(n.elements, child.Elements()...)
	}
	n.childKeys = nil
}

func withRemoved(elements []*Zp, z *Zp) (result []*Zp) {
	var has bool
	for _, element := range elements {
		if element.Cmp(z) != 0 {
			result = append(result, element)
		} else {
			has = true
		}
	}
	if !has {
		panic("Remove non-existent element from node")
	}
	return
}