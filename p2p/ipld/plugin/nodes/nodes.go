package nodes

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-ipfs/core/coredag"
	"github.com/ipfs/go-ipfs/plugin"
	format "github.com/ipfs/go-ipld-format"
	node "github.com/ipfs/go-ipld-format"
	"github.com/lazyledger/nmt"
	mh "github.com/multiformats/go-multihash"
)

const (
	// Below used multiformats (one codec, one multihash) seem free:
	// https://github.com/multiformats/multicodec/blob/master/table.csv

	// Nmt is the codec used for leaf and inner nodes of an Namespaced Merkle Tree.
	Nmt = 0x7700

	// NmtCodecName is the name used during registry of the Nmt codec
	NmtCodecName = "nmt-node"

	// Sha256Namespace8Flagged is the multihash code used to hash blocks
	// that contain an NMT node (inner and leaf nodes).
	Sha256Namespace8Flagged = 0x7701

	// DagParserFormatName can be used when putting into the IPLD Dag
	DagParserFormatName = "extended-square-row-or-col"

	// FIXME: These are the same as types.ShareSize and types.NamespaceSize.
	// Repeated here to avoid a dependency to the wrapping repo as this makes
	// it hard to compile and use the plugin against a local ipfs version.
	// TODO: plugins have config options; make this configurable instead
	namespaceSize = 8
	shareSize     = 256
	// nmtHashSize is the size of a digest created by an NMT in bytes.
	nmtHashSize = 2*namespaceSize + sha256.Size
)

func init() {
	mustRegisterNamespacedCodec(
		Sha256Namespace8Flagged,
		"sha2-256-namespace8-flagged",
		nmtHashSize,
		sumSha256Namespace8Flagged,
	)
	// this should already happen when the plugin is injected but it doesn't for some CI tests
	format.DefaultBlockDecoder.Register(Nmt, NmtNodeParser)
	// register the codecs in the global maps
	cid.Codecs[NmtCodecName] = Nmt
	cid.CodecToStr[Nmt] = NmtCodecName
}

func mustRegisterNamespacedCodec(
	codec uint64,
	name string,
	defaultLength int,
	hashFunc mh.HashFunc,
) {
	if _, ok := mh.Codes[codec]; !ok {
		// make sure that the Codec wasn't registered from somewhere different than this plugin already:
		if _, found := mh.Codes[codec]; found {
			panic(fmt.Sprintf("Codec 0x%X is already present: %v", codec, mh.Codes[codec]))
		}
		// add to mh.Codes map first, otherwise mh.RegisterHashFunc would err:
		mh.Codes[codec] = name
		mh.Names[name] = codec
		mh.DefaultLengths[codec] = defaultLength

		if err := mh.RegisterHashFunc(codec, hashFunc); err != nil {
			panic(fmt.Sprintf("could not register hash function: %v", mh.Codes[codec]))
		}
	}
}

// sumSha256Namespace8Flagged is the mh.HashFunc used to hash leaf and inner nodes.
// It is registered as a mh.HashFunc in the go-multihash module.
func sumSha256Namespace8Flagged(data []byte, _length int) ([]byte, error) {
	isLeafData := data[0] == nmt.LeafPrefix
	if isLeafData {
		return nmt.Sha256Namespace8FlaggedLeaf(data[1:]), nil
	}
	return nmt.Sha256Namespace8FlaggedInner(data[1:]), nil
}

var Plugins = []plugin.Plugin{&LazyLedgerPlugin{}}

var _ plugin.PluginIPLD = &LazyLedgerPlugin{}

type LazyLedgerPlugin struct{}

func (l LazyLedgerPlugin) RegisterBlockDecoders(dec format.BlockDecoder) error {
	dec.Register(Nmt, NmtNodeParser)
	return nil
}

func (l LazyLedgerPlugin) RegisterInputEncParsers(iec coredag.InputEncParsers) error {
	iec.AddParser("raw", DagParserFormatName, DataSquareRowOrColumnRawInputParser)
	return nil
}

func (l LazyLedgerPlugin) Name() string {
	return "LazyLedger"
}

func (l LazyLedgerPlugin) Version() string {
	return "0.0.0"
}

func (l LazyLedgerPlugin) Init(env *plugin.Environment) error {
	return nil
}

// DataSquareRowOrColumnRawInputParser reads the raw shares and extract the IPLD nodes from the NMT tree.
// Note, to parse without any error the input has to be of the form:
//
// <share_0>| ... |<share_numOfShares - 1>
//
// To determine the share and the namespace size the constants
// types.ShareSize and types.NamespaceSize are redefined here to avoid
// lazyledger-core as a dependency.
//
// Note while this coredag.DagParser is implemented here so this plugin can be used from
// the commandline, the ipld Nodes will rather be created together with the NMT
// root instead of re-computing it here.
func DataSquareRowOrColumnRawInputParser(r io.Reader, _mhType uint64, _mhLen int) ([]node.Node, error) {
	br := bufio.NewReader(r)
	collector := newNodeCollector()

	n := nmt.New(
		sha256.New(),
		nmt.NamespaceIDSize(namespaceSize),
		nmt.NodeVisitor(collector.visit),
	)

	for {
		namespacedLeaf := make([]byte, shareSize+namespaceSize)
		if _, err := io.ReadFull(br, namespacedLeaf); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if err := n.Push(namespacedLeaf[:namespaceSize], namespacedLeaf[namespaceSize:]); err != nil {
			return nil, err
		}
	}
	// to trigger the collection of nodes:
	_ = n.Root()
	return collector.ipldNodes(), nil
}

// nmtNodeCollector creates and collects ipld.Nodes if inserted into a nmt tree.
// It is mainly used for testing.
type nmtNodeCollector struct {
	nodes []node.Node
}

func newNodeCollector() *nmtNodeCollector {
	// The extendedRowOrColumnSize is hardcode this here to avoid importing:
	// https://github.com/lazyledger/lazyledger-core/blob/585566317e519bbb6d35d149b7e856c4c1e8657c/types/consts.go#L23
	const extendedRowOrColumnSize = 2 * 128
	return &nmtNodeCollector{nodes: make([]node.Node, 0, extendedRowOrColumnSize)}
}

func (n nmtNodeCollector) ipldNodes() []node.Node {
	return n.nodes
}

func (n *nmtNodeCollector) visit(hash []byte, children ...[]byte) {
	cid := mustCidFromNamespacedSha256(hash)
	switch len(children) {
	case 1:
		n.nodes = prependNode(nmtLeafNode{
			cid:  cid,
			Data: children[0],
		}, n.nodes)
	case 2:
		n.nodes = prependNode(nmtNode{
			cid: cid,
			l:   children[0],
			r:   children[1],
		}, n.nodes)
	default:
		panic("expected a binary tree")
	}
}

func prependNode(newNode node.Node, nodes []node.Node) []node.Node {
	nodes = append(nodes, node.Node(nil))
	copy(nodes[1:], nodes)
	nodes[0] = newNode
	return nodes
}

// NmtNodeAdder adds ipld.Nodes to the underlying ipld.Batch if it is inserted
// into an nmt tree
type NmtNodeAdder struct {
	batch *format.Batch
	ctx   context.Context
}

// NewNmtNodeAdder returns a new NmtNodeAdder with the provided context and
// batch. Note that the context provided should have a timeout
func NewNmtNodeAdder(ctx context.Context, batch *format.Batch) *NmtNodeAdder {
	return &NmtNodeAdder{
		batch: batch,
		ctx:   ctx,
	}
}

// Visit can be inserted into an nmt tree to create ipld.Nodes while computing the root
func (n *NmtNodeAdder) Visit(hash []byte, children ...[]byte) {
	cid := mustCidFromNamespacedSha256(hash)
	switch len(children) {
	case 1:
		n.batch.Add(n.ctx, nmtLeafNode{
			cid:  cid,
			Data: children[0],
		})
	case 2:
		n.batch.Add(n.ctx, nmtNode{
			cid: cid,
			l:   children[0],
			r:   children[1],
		})
	default:
		panic("expected a binary tree")
	}
}

// Batch return the ipld.Batch originally provided to the NmtNodeAdder
func (n *NmtNodeAdder) Batch() *format.Batch {
	return n.batch
}

func NmtNodeParser(block blocks.Block) (node.Node, error) {
	// length of the domain separator for leaf and inner nodes:
	const prefixOffset = 1
	var (
		leafPrefix  = []byte{nmt.LeafPrefix}
		innerPrefix = []byte{nmt.NodePrefix}
	)
	data := block.RawData()
	if len(data) == 0 {
		return &nmtLeafNode{
			cid:  cid.Undef,
			Data: nil,
		}, nil
	}
	domainSeparator := data[:prefixOffset]
	if bytes.Equal(domainSeparator, leafPrefix) {
		return &nmtLeafNode{
			cid:  block.Cid(),
			Data: data[prefixOffset:],
		}, nil
	}
	if bytes.Equal(domainSeparator, innerPrefix) {
		return nmtNode{
			cid: block.Cid(),
			l:   data[prefixOffset : prefixOffset+nmtHashSize],
			r:   data[prefixOffset+nmtHashSize:],
		}, nil
	}
	return nil, fmt.Errorf(
		"expected first byte of block to be either the leaf or inner node prefix: (%x, %x), got: %x)",
		leafPrefix,
		innerPrefix,
		domainSeparator,
	)
}

var _ node.Node = (*nmtNode)(nil)
var _ node.Node = (*nmtLeafNode)(nil)

type nmtNode struct {
	// TODO(ismail): we might want to export these later
	cid  cid.Cid
	l, r []byte
}

func (n nmtNode) RawData() []byte {
	return append([]byte{nmt.NodePrefix}, append(n.l, n.r...)...)
}

func (n nmtNode) Cid() cid.Cid {
	return n.cid
}

func (n nmtNode) String() string {
	return fmt.Sprintf(`
node {
	hash: %x,
	l: %x,
	r: %x"
}`, n.cid.Hash(), n.l, n.r)
}

func (n nmtNode) Loggable() map[string]interface{} {
	return nil
}

func (n nmtNode) Resolve(path []string) (interface{}, []string, error) {
	switch path[0] {
	case "0":
		left, err := CidFromNamespacedSha256(n.l)
		if err != nil {
			return nil, nil, err
		}
		return &node.Link{Cid: left}, path[1:], nil
	case "1":
		right, err := CidFromNamespacedSha256(n.r)
		if err != nil {
			return nil, nil, err
		}
		return &node.Link{Cid: right}, path[1:], nil
	default:
		return nil, nil, errors.New("invalid path for inner node")
	}
}

func (n nmtNode) Tree(path string, depth int) []string {
	if path != "" || depth != -1 {
		panic("proper tree not yet implemented")
	}

	return []string{
		"0",
		"1",
	}
}

func (n nmtNode) ResolveLink(path []string) (*node.Link, []string, error) {
	obj, rest, err := n.Resolve(path)
	if err != nil {
		return nil, nil, err
	}

	lnk, ok := obj.(*node.Link)
	if !ok {
		return nil, nil, errors.New("was not a link")
	}

	return lnk, rest, nil
}

func (n nmtNode) Copy() node.Node {
	l := make([]byte, len(n.l))
	copy(l, n.l)
	r := make([]byte, len(n.r))
	copy(r, n.r)

	return &nmtNode{
		cid: n.cid,
		l:   l,
		r:   r,
	}
}

func (n nmtNode) Links() []*node.Link {
	leftCid := mustCidFromNamespacedSha256(n.l)
	rightCid := mustCidFromNamespacedSha256(n.r)

	return []*node.Link{{Cid: leftCid}, {Cid: rightCid}}
}

func (n nmtNode) Stat() (*node.NodeStat, error) {
	return &node.NodeStat{}, nil
}

func (n nmtNode) Size() (uint64, error) {
	return 0, nil
}

type nmtLeafNode struct {
	cid  cid.Cid
	Data []byte
}

func (l nmtLeafNode) RawData() []byte {
	return append([]byte{nmt.LeafPrefix}, l.Data...)
}

func (l nmtLeafNode) Cid() cid.Cid {
	return l.cid
}

func (l nmtLeafNode) String() string {
	return fmt.Sprintf(`
leaf {
	hash: 		%x,
	len(Data): 	%v
}`, l.cid.Hash(), len(l.Data))
}

func (l nmtLeafNode) Loggable() map[string]interface{} {
	return nil
}

func (l nmtLeafNode) Resolve(path []string) (interface{}, []string, error) {
	return nil, nil, errors.New("invalid path for leaf node")
}

func (l nmtLeafNode) Tree(_path string, _depth int) []string {
	return nil
}

func (l nmtLeafNode) ResolveLink(path []string) (*node.Link, []string, error) {
	obj, rest, err := l.Resolve(path)
	if err != nil {
		return nil, nil, err
	}

	lnk, ok := obj.(*node.Link)
	if !ok {
		return nil, nil, errors.New("was not a link")
	}
	return lnk, rest, nil
}

func (l nmtLeafNode) Copy() node.Node {
	panic("implement me")
}

func (l nmtLeafNode) Links() []*node.Link {
	return []*node.Link{{Cid: l.Cid()}}
}

func (l nmtLeafNode) Stat() (*node.NodeStat, error) {
	return &node.NodeStat{}, nil
}

func (l nmtLeafNode) Size() (uint64, error) {
	return 0, nil
}

// CidFromNamespacedSha256 uses a hash from an nmt tree to create a cide
func CidFromNamespacedSha256(namespacedHash []byte) (cid.Cid, error) {
	if got, want := len(namespacedHash), nmtHashSize; got != want {
		return cid.Cid{}, fmt.Errorf("invalid namespaced hash length, got: %v, want: %v", got, want)
	}
	buf, err := mh.Encode(namespacedHash, Sha256Namespace8Flagged)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(Nmt, mh.Multihash(buf)), nil
}

// mustCidFromNamespacedSha256 is a wrapper around cidFromNamespacedSha256 that panics
// in case of an error. Use with care and only in places where no error should occur.
func mustCidFromNamespacedSha256(hash []byte) cid.Cid {
	cid, err := CidFromNamespacedSha256(hash)
	if err != nil {
		panic(
			fmt.Sprintf("malformed hash: %s, codec: %v",
				err,
				mh.Codes[Sha256Namespace8Flagged]),
		)
	}
	return cid
}
