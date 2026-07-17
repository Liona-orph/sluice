package llm

// commonWords is the word list Approx segments against.
//
// It is roughly the most frequent few hundred English words plus the technical
// vocabulary that dominates LLM traffic in practice (API, request, model,
// database, function). Frequency is what matters: BPE vocabularies allocate
// whole-word tokens by corpus frequency, so a list ordered the same way tracks
// their behaviour without needing their merge table. Extending it is safe --
// entries only ever reduce a word to one token -- and adding a rare word makes
// the estimate worse, not better, because a rare word is not a single token
// upstream either.
//
// Kept as a space-separated literal rather than a slice of strings so the diff
// when someone extends it is readable.
const commonWords = `
a about above accept access account across act action active actually add address
after again against age ago agree ahead all allow almost alone along already also
always am among amount an analysis and animal another answer any anything api app
appear application apply approach are area around array art article as ask assume
at attack attempt attention author auth available average avoid away back bad bag
balance bank base basic be beautiful because become been before begin behind being
believe below best better between beyond big bit black block blog blue board body
book both bottom box boy break bring browser budget build building business but buy
by cache call came camera can cannot capital car card care carry case cash catch
cause cell center central century certain chance change channel chapter character
charge chart check child choice choose church city civil claim class clean clear
click client close cloud club code cold collect college color column come command
comment common community company compare complete computer concern condition
config configure confirm connect consider constant contain content context continue
control cook copy core corner correct cost could count country couple course cover
create credit crisis critical cross culture current custom customer cut daily damage
dance danger dark data database date daughter day deal death debug decade decide
decision deep default defense define degree delete deliver demand democracy depend
describe design despite detail detect determine develop device did die difference
different difficult digital direct direction director discover discuss disease
display distance do doctor document does dog domain done door double down draw
dream drive drop drug due during each early earth ease east easy eat economic
economy edge edit education effect effort eight either election element else email
employee encode end endpoint energy engine engineer english enjoy enough enter
entire entity environment equal error especially establish even evening event ever
every everyone everything evidence exactly example except exist expect experience
expert explain export express extend eye face fact factor fail fall false family
far fast father fear feature federal feel few field fight figure file fill film
filter final finally financial find fine finger finish fire firm first fish fit
five fix flag flat floor flow fly focus follow food foot for force foreign forget
form format former forward found four frame free french fresh friend from front
full function fund future gain game gap garden gas gather general generate german
get gift girl give glass global go goal god gold gone good got government great
green ground group grow growth guess guest guide gun guy had half hand handle
hang happen happy hard has hash have he head health hear heart heat heavy hello
help her here herself high him himself his history hit hold hole home hope
horse hospital host hot hotel hour house how however http huge human hundred
husband i idea identify if ignore image imagine impact implement important improve
in include increase indeed index indicate individual industry info information
initial input inside instead institution integer interest interface internal
international internet interview into invalid invoice involve is issue it item its
itself java job join json judge jump just keep key kid kill kind kitchen know
knowledge lab label lack land language large last late later laugh law lawyer lay
layer lead leader learn least leave left leg legal length less let letter level
library lie life light like likely limit line link list listen literature little
live load loan local locate lock log logic long look loop lose loss lost lot love
low machine magazine main maintain major make man manage manager many map market
marriage master match material matter may maybe me mean measure media medical meet
member memory mention message method middle might military million mind mine
minute miss mission model modern modify moment money monitor month more morning
most mother mouth move movie much music must my myself name nation national
natural nature near nearly necessary need network never new news next nice night
no node none nor normal north not note nothing notice now null number object
observe occur ocean of off offer office officer official often oh oil okay old on
once one only onto open operate operation opportunity option or order org organize
origin other others otherwise our out output outside over own owner packet page
paid pain paint pair panel paper parameter parent park parse part particular
partner party pass password past patch path patient pattern pay payment peace
people per percent perfect perform performance perhaps period permission person
personal phone photo physical pick picture piece place plan plant play player
please point police policy political poll pool poor pop popular population port
position positive possible post pound power practice prefer prepare present
president press pressure pretty prevent previous price primary print prior private
probably problem process produce product professional program project promise
property propose protect protocol prove provide provider public pull purpose push
put quality query question queue quick quickly quite race radio raise range rate
rather reach read reader ready real reality realize really reason receive recent
recognize record red reduce refer reference reflect region relate relationship
release religious remain remember remove repeat replace reply report represent
request require research resource respond response responsible rest restore result
retry return reveal review rich right rise risk road rock role roll room root round
route row rule run safe safety salt same sample save say scale scene schedule
school science score script sea search season seat second secret section secure
security see seek seem select sell send senior sense sentence separate serious serve
server service session set setting seven several sex shake shape share she shoot
shop short shot should shoulder show side sign signal significant similar simple
simply since sing single sir sister sit site situation six size skill skin sleep
slow small smile so social society soft software solution solve some someone
something sometimes son song soon sort sound source south space speak special
specific speech speed spend split sport spring sql staff stage stand standard star
start state statement station status stay step still stock stop store story straight
strategy stream street strong structure student study stuff style subject submit
success such sudden suffer suggest summer sun supply support suppose sure surface
switch symbol system table take talk target task tax teach teacher team tech
technology tell template ten term test text than thank that the their them
themselves then theory there these they thing think third this those though thought
thousand three through throw thus ticket time tiny title to today together token
tonight too tool top topic total touch toward town trace track trade traffic train
transfer travel treat tree trial trip trouble true trust truth try turn tv twenty
two type under understand union unit unless until up update upon usage use user
usually valid value variable various version very via video view visit voice vote
wait walk wall want war warm warn wash watch water way we weak wear web week weight
welcome well west what whatever when where whether which while white who whole whom
whose why wide wife will win wind window wine winter wish with within without woman
women wonder word work worker world worry would write writer wrong xml yard yeah
year yes yesterday yet you young your yourself zero zone
`

// commonProper are proper nouns frequent enough that a BPE vocabulary carries
// them whole: countries, large cities, languages, and the brands that appear in
// technical prose. Without them a country name degrades into two tokens and the
// error shows up on exactly the kind of factual question people send to a
// gateway.
const commonProper = `
africa amazon america american android apple arabic asia atlanta australia austin
berlin boston brazil britain british canada chicago china chinese cloudflare
denver detroit dublin egypt england europe european facebook florida france
french germany google greece hindi houston india indian ireland israel italy
italian japan japanese jersey kenya korea korean lisbon london madrid manchester
mexico miami microsoft moscow mumbai munich netherlands nigeria norway openai
oregon oslo ottawa pakistan paris phoenix poland portugal russia russian seattle
seoul shanghai singapore spain spanish stockholm sweden swedish switzerland
sydney taiwan texas thailand tokyo toronto turkey ukraine vancouver vienna
vietnam virginia warsaw washington zurich
alice andrew anna barbara carlos charles daniel david elena emily george helen
james jennifer john joseph julia karen laura linda maria mark martin mary michael
nancy oliver patricia paul peter richard robert sarah simon sophia steven susan
thomas william
android chrome docker github gmail google java kubernetes linux macos openai
oracle python postgres redis safari ubuntu windows
`

// commonFragments are subword pieces used when no whole word matches. They are
// the affixes a BPE merge table would have learned early, and they keep an
// unfamiliar word from degrading straight to the four-characters-per-token
// fallback.
const commonFragments = `
abil able ably ance ancy ant ary ate ation ative ces cion cious ction dis eer ence
ency ent eous ery esque ess est ful gram graph hood ial ian ibility ible ic ical
ify ily ine ing ion ious ise ish ism ist ite itive ity ive ization ize izer less
let like ling logy ment ness ology ous over pre pro ship sion sive tion tive trans
ture ual ular under uous ure ward ware wise
`
