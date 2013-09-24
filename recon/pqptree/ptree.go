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

// Package pqptree provides a PostgreSQL-backed implementation of the
// recon prefix tree interface.
package pqptree

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	. "github.com/cmars/conflux"
	"github.com/cmars/conflux/recon"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"log"
	"strconv"
	"strings"
	"text/template"
)

type PNode struct {
	NodeKey        string `db:"node_key"`
	SValues        string `db:"svalues"`
	NumElements    int    `db:"num_elements"`
	ChildKeyString string `db:"child_keys"`
	childKeys      []int
	elements       []PElement
}

type PElement struct {
	NodeKey string `db:"node_key"`
	Element []byte `db:"element"`
}

type pqPrefixTree struct {
	*Settings
	Namespace                string
	root                     *PNode
	db                       *sqlx.DB
	points                   []*Zp
	selectPNodeByNodeKey     string
	selectPElementsByNodeKey string
	deletePNode              string
	deletePElements          string
	deletePElement           string
	insertPElement           string
	updatePElement           string
	insertNewPNode           string
	updatePNode              string
}

type pqPrefixNode struct {
	*pqPrefixTree
	*PNode
}

func mustEncodeBitstring(bs *Bitstring) string {
	w := bytes.NewBuffer(nil)
	err := recon.WriteBitstring(w, bs)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(w.Bytes())
}

func mustDecodeBitstring(enc string) *Bitstring {
	buf, err := hex.DecodeString(enc)
	if err != nil {
		panic(err)
	}
	bs, err := recon.ReadBitstring(bytes.NewBuffer(buf))
	if err != nil {
		panic(err)
	}
	return bs
}

func mustEncodeZZarray(arr []*Zp) string {
	w := bytes.NewBuffer(nil)
	err := recon.WriteZZarray(w, arr)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(w.Bytes())
}

func mustDecodeZZarray(enc string) []*Zp {
	buf, err := hex.DecodeString(enc)
	if err != nil {
		panic(err)
	}
	arr, err := recon.ReadZZarray(bytes.NewBuffer(buf))
	if err != nil {
		panic(err)
	}
	return arr
}

func New(namespace string, db *sqlx.DB, settings *Settings) (ptree recon.PrefixTree, err error) {
	tree := &pqPrefixTree{
		Settings:  settings,
		Namespace: namespace,
		db:        db,
		points:    Zpoints(P_SKS, settings.NumSamples())}
	tree.prepareStatements()
	ptree = tree
	return
}

func (t *pqPrefixTree) SqlTemplate(sql string) string {
	result := bytes.NewBuffer(nil)
	err := template.Must(template.New("sql").Parse(sql)).Execute(result, t)
	if err != nil {
		panic(err)
	}
	return result.String()
}

func (t *pqPrefixTree) Create() error {
	var err error
	for i, tmpl := range []string{
		CreateTable_PNode, CreateTable_PElement, CreateIndex_PElement_NodeKey} {
		sql := t.SqlTemplate(tmpl)
		log.Println(sql)
		if _, err = t.db.Execv(sql); err != nil {
			log.Println(err)
			if i < 2 {
				return err
			}
		}
	}
	return t.ensureRoot()
}

func (t *pqPrefixTree) Drop() error {
	var err error
	for _, tmpl := range []string{
		DropIndex_PElement_NodeKey, DropTable_PElement, DropTable_PNode} {
		sql := t.SqlTemplate(tmpl)
		log.Println(sql)
		if _, err = t.db.Execv(sql); err != nil {
			log.Println(err)
		}
	}
	return nil
}

func (t *pqPrefixTree) prepareStatements() {
	t.selectPNodeByNodeKey = t.SqlTemplate(
		"SELECT * FROM {{.Namespace}}_pnode WHERE node_key = $1")
	t.selectPElementsByNodeKey = t.SqlTemplate(
		"SELECT * FROM {{.Namespace}}_pelement WHERE node_key = $1")
	t.deletePNode = t.SqlTemplate(
		"DELETE FROM {{.Namespace}}_pnode WHERE node_key = $1")
	t.deletePElements = t.SqlTemplate(
		`DELETE FROM {{.Namespace}}_pelement WHERE node_key = $1`)
	t.deletePElement = t.SqlTemplate(`
DELETE FROM {{.Namespace}}_pelement WHERE element = $1
RETURNING *`)
	t.insertPElement = t.SqlTemplate(`
INSERT INTO {{.Namespace}}_pelement (node_key, element)
VALUES ($1, $2)`)
	t.updatePElement = t.SqlTemplate(`
UPDATE {{.Namespace}}_pelement SET node_key = $1 WHERE element = $2`)
	t.insertNewPNode = t.SqlTemplate(`
INSERT INTO {{.Namespace}}_pnode (node_key, svalues, num_elements, child_keys)
SELECT $1, $2, $3, $4 WHERE NOT EXISTS (
SELECT 1 FROM {{.Namespace}}_pnode WHERE node_key = $1)
RETURNING *`)
	t.updatePNode = t.SqlTemplate(`
UPDATE {{.Namespace}}_pnode
SET svalues = $2, num_elements = $3, child_keys = $4
WHERE node_key = $1`)
}

func (t *pqPrefixTree) Init() {
}

func (t *pqPrefixTree) ensureRoot() (err error) {
	_, err = t.Root()
	if err != recon.PNodeNotFound {
		return
	}
	root := t.newChildNode(nil, 0)
	return root.upsertNode()
}

func (t *pqPrefixTree) Points() []*Zp { return t.points }

func (t *pqPrefixTree) Root() (recon.PrefixNode, error) {
	return t.Node(NewBitstring(0))
}

func decodeIntArray(s string) ([]int, error) {
	s = strings.Trim(s, "{}")
	var result []int
	for _, istr := range strings.Split(s, ",") {
		if len(istr) > 0 {
			i, err := strconv.Atoi(istr)
			if err != nil {
				return nil, err
			}
			result = append(result, i)
		}
	}
	return result, nil
}

func encodeIntArray(iarr []int) string {
	b := bytes.NewBuffer(nil)
	fmt.Fprintf(b, "{")
	for i, ival := range iarr {
		if i > 0 {
			fmt.Fprintf(b, ",")
		}
		fmt.Fprintf(b, "%d", ival)
	}
	fmt.Fprintf(b, "}")
	return b.String()
}

func (t *pqPrefixTree) Node(bs *Bitstring) (recon.PrefixNode, error) {
	nodeKey := mustEncodeBitstring(bs)
	node := &pqPrefixNode{PNode: &PNode{}, pqPrefixTree: t}
	err := t.db.Get(node.PNode, t.selectPNodeByNodeKey, nodeKey)
	if err == sql.ErrNoRows {
		return nil, recon.PNodeNotFound
	} else if err != nil {
		return nil, err
	}
	node.childKeys, err = decodeIntArray(node.ChildKeyString)
	if err != nil {
		return nil, err
	}
	err = t.db.Select(&node.PNode.elements, t.selectPElementsByNodeKey, nodeKey)
	if err == sql.ErrNoRows {
		err = nil
	}
	return node, err
}

type elementOperation func() (bool, error)

type changeElement struct {
	// Current node in prefix tree descent
	cur *pqPrefixNode
	// Element to be changed (added or removed)
	element *Zp
	// Mask used to update sample values
	marray []*Zp
	// Target prefix tree key to shoot for
	target *Bitstring
	// Current depth in descent
	depth int
}

func (ch *changeElement) descend(op elementOperation) error {
	for {
		ch.cur.updateSvalues(ch.element, ch.marray)
		done, err := op()
		if done || err != nil {
			return err
		}
	}
}

func (ch *changeElement) insert() (done bool, err error) {
	ch.cur.NumElements++
	if ch.cur.IsLeaf() {
		if len(ch.cur.elements) > ch.cur.SplitThreshold() {
			err = ch.split()
			if err != nil {
				return
			}
		} else {
			ch.cur.upsertNode()
			err = ch.cur.insertElement(ch.element)
			return err == nil, err
		}
	}
	ch.cur.upsertNode()
	childIndex := recon.NextChild(ch.cur, ch.target, ch.depth)
	ch.cur = ch.cur.Children()[childIndex].(*pqPrefixNode)
	ch.depth++
	return false, err
}

func (n *pqPrefixNode) deleteNode() error {
	err := n.deleteElements()
	if err != nil {
		return err
	}
	_, err = n.db.Execv(n.deletePNode, n.NodeKey)
	return err
}

func (n *pqPrefixNode) deleteElements() error {
	_, err := n.db.Execv(n.deletePElements, n.NodeKey)
	if err != nil {
		return err
	}
	n.elements = []PElement{}
	return nil
}

func (n *pqPrefixNode) deleteElement(element *Zp) error {
	elementBytes := element.Bytes()
	_, err := n.db.Execv(n.deletePElement, elementBytes)
	if err != nil {
		return err
	}
	var elements []PElement
	for _, element := range n.elements {
		if !bytes.Equal(element.Element, elementBytes) {
			elements = append(elements, element)
		}
	}
	n.elements = elements
	return err
}

func (n *pqPrefixNode) insertElement(element *Zp) error {
	_, err := n.db.Execv(n.insertPElement, n.NodeKey, element.Bytes())
	if err != nil {
		return err
	}
	n.elements = append(n.elements, PElement{NodeKey: n.PNode.NodeKey, Element: element.Bytes()})
	return err
}

func (ch *changeElement) split() (err error) {
	// Create child nodes
	numChildren := 1 << uint(ch.cur.BitQuantum())
	var children []*pqPrefixNode
	for i := 0; i < numChildren; i++ {
		// Create new empty child node
		child := ch.cur.newChildNode(ch.cur, i)
		err = child.upsertNode()
		if err != nil {
			return err
		}
		ch.cur.childKeys = append(ch.cur.childKeys, i)
		children = append(children, child)
	}
	err = ch.cur.upsertNode()
	if err != nil {
		return err
	}
	// Move elements into child nodes
	for _, element := range ch.cur.elements {
		z := Zb(P_SKS, element.Element)
		bs := NewZpBitstring(z)
		childIndex := recon.NextChild(ch.cur, bs, ch.depth)
		child := children[childIndex]
		_, err = child.db.Execv(child.updatePElement, child.NodeKey, element.Element)
		marray, err := recon.AddElementArray(child, z)
		if err != nil {
			return err
		}
		child.updateSvalues(z, marray)
	}
	for _, child := range children {
		err = child.upsertNode()
		if err != nil {
			return err
		}
	}
	return
}

func (ch *changeElement) remove() (done bool, err error) {
	ch.cur.NumElements--
	if !ch.cur.IsLeaf() {
		if ch.cur.NumElements <= ch.cur.JoinThreshold() {
			err = ch.join()
			if err != nil {
				return
			}
		} else {
			err = ch.cur.upsertNode()
			if err != nil {
				return
			}
			childIndex := recon.NextChild(ch.cur, ch.target, ch.depth)
			ch.cur = ch.cur.Children()[childIndex].(*pqPrefixNode)
			ch.depth++
			return false, err
		}
	}
	if err = ch.cur.upsertNode(); err != nil {
		return
	}
	err = ch.cur.deleteElement(ch.element)
	return err == nil, err
}

func (ch *changeElement) join() error {
	var elements []PElement
	for _, child := range ch.cur.Children() {
		elements = append(elements, child.(*pqPrefixNode).elements...)
		for _, element := range child.(*pqPrefixNode).elements {
			_, err := ch.cur.db.Execv(ch.cur.updatePElement, ch.cur.NodeKey, element.Element)
			if err != nil {
				return err
			}
		}
		err := child.(*pqPrefixNode).deleteNode()
		if err != nil {
			return err
		}
	}
	ch.cur.childKeys = nil
	return ch.cur.upsertNode()
}

func (t *pqPrefixTree) HasElement(z *Zp) (bool, error) {
	var result struct {
		Count int
	}
	err := t.db.Get(&result, t.SqlTemplate(`
SELECT COUNT(*) FROM {{.Namespace}}_pelement WHERE element = $1`), z.Bytes())
	if err != nil {
		return false, err
	}
	return result.Count > 0, nil
}

func ErrDuplicateElement(z *Zp) error {
	return errors.New(fmt.Sprintf("Attempt to insert duplicate element %v", z))
}

func (t *pqPrefixTree) Insert(z *Zp) error {
	if has, err := t.HasElement(z); has {
		return ErrDuplicateElement(z)
	} else if err != nil {
		return err
	}
	bs := NewZpBitstring(z)
	root, err := t.Root()
	if err != nil {
		return err
	}
	marray, err := recon.AddElementArray(t, z)
	if err != nil {
		return err
	}
	ch := &changeElement{
		cur:     root.(*pqPrefixNode),
		element: z,
		marray:  marray,
		target:  bs}
	return ch.descend(ch.insert)
}

func (t *pqPrefixTree) Remove(z *Zp) error {
	if has, err := t.HasElement(z); !has {
		return recon.PNodeNotFound
	} else if err != nil {
		return err
	}
	bs := NewZpBitstring(z)
	root, err := t.Root()
	if err != nil {
		return err
	}
	ch := &changeElement{
		cur:     root.(*pqPrefixNode),
		element: z,
		marray:  recon.DelElementArray(t, z),
		target:  bs}
	return ch.descend(ch.remove)
}

func (t *pqPrefixTree) newChildNode(parent *pqPrefixNode, childIndex int) *pqPrefixNode {
	n := &pqPrefixNode{pqPrefixTree: t, PNode: &PNode{}}
	var key *Bitstring
	if parent != nil {
		parentKey := parent.Key()
		key = NewBitstring(parentKey.BitLen() + t.BitQuantum())
		key.SetBytes(parentKey.Bytes())
		for j := 0; j < parent.BitQuantum(); j++ {
			if (childIndex>>uint(j))&0x1 == 1 {
				key.Set(parentKey.BitLen() + j)
			} else {
				key.Unset(parentKey.BitLen() + j)
			}
		}
	} else {
		key = NewBitstring(0)
	}
	n.PNode.NodeKey = mustEncodeBitstring(key)
	svalues := make([]*Zp, t.NumSamples())
	for i := 0; i < len(svalues); i++ {
		svalues[i] = Zi(P_SKS, 1)
	}
	n.PNode.SValues = mustEncodeZZarray(svalues)
	return n
}

func (n *pqPrefixNode) upsertNode() error {
	n.ChildKeyString = encodeIntArray(n.childKeys)
	rs, err := n.db.Execv(n.insertNewPNode,
		n.NodeKey, n.PNode.SValues, n.NumElements, n.ChildKeyString)
	if err != nil {
		return err
	}
	nrows, err := rs.RowsAffected()
	if err != nil {
		return err
	}
	if nrows == 0 {
		_, err = n.db.Execv(n.updatePNode,
			n.NodeKey, n.PNode.SValues, n.NumElements, n.ChildKeyString)
	}
	return err
}

func (n *pqPrefixNode) IsLeaf() bool {
	return len(n.childKeys) == 0
}

func (n *pqPrefixNode) Children() (result []recon.PrefixNode) {
	key := n.Key()
	for _, i := range n.childKeys {
		childKey := NewBitstring(key.BitLen() + n.BitQuantum())
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
			panic(fmt.Sprintf("Children failed on child#%v, key=%v: %v", i, childKey, err))
		}
		result = append(result, child)
	}
	return
}

func (n *pqPrefixNode) Elements() (result []*Zp) {
	for _, element := range n.elements {
		result = append(result, Zb(P_SKS, element.Element))
	}
	return
}

func (n *pqPrefixNode) Size() int { return n.NumElements }

func (n *pqPrefixNode) SValues() []*Zp {
	return mustDecodeZZarray(n.PNode.SValues)
}

func (n *pqPrefixNode) Key() *Bitstring {
	return mustDecodeBitstring(n.NodeKey)
}

func (n *pqPrefixNode) Parent() (recon.PrefixNode, bool) {
	key := n.Key()
	if key.BitLen() == 0 {
		return nil, false
	}
	parentKey := NewBitstring(key.BitLen() - n.BitQuantum())
	parentKey.SetBytes(key.Bytes())
	parent, err := n.Node(parentKey)
	if err != nil {
		panic(fmt.Sprintf("Failed to get parent: %v", err))
	}
	return parent, true
}

func (n *pqPrefixNode) updateSvalues(z *Zp, marray []*Zp) {
	if len(marray) != len(n.points) {
		panic("Inconsistent NumSamples size")
	}
	svalues := mustDecodeZZarray(n.PNode.SValues)
	for i := 0; i < len(marray); i++ {
		svalues[i] = Z(z.P).Mul(svalues[i], marray[i])
	}
	/*
		log.Println("update svalues:")
		for _, zi := range svalues {
			log.Println(hex.EncodeToString(zi.Bytes()))
		}
	*/
	n.PNode.SValues = mustEncodeZZarray(svalues)
}